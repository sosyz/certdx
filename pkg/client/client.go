package client

import (
	"bytes"
	"os"
	"os/signal"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"pkg.para.party/certdx/pkg/config"
	"pkg.para.party/certdx/pkg/logging"
	"pkg.para.party/certdx/pkg/types"
	"pkg.para.party/certdx/pkg/utils"
)

type CertDXClientDaemon struct {
	Config    *config.ClientConfigT
	ClientOpt []CertDXHttpClientOption

	certs []*watchingCert
	wg    sync.WaitGroup
}

type certData struct {
	Domains        []string
	Fullchain, Key []byte
}

type watchingCert struct {
	Data           certData
	Config         config.ClientCertification
	UpdateHandlers []certUpdateHandler
	UpdateChan     chan certData
	Stop           atomic.Pointer[chan struct{}]
}

func (c *watchingCert) watch(wg *sync.WaitGroup) {
	wg.Add(1)
	defer wg.Done()
	for {
		select {
		case <-*c.Stop.Load():
			return
		case newCert := <-c.UpdateChan:
			if !bytes.Equal(c.Data.Fullchain, newCert.Fullchain) || !bytes.Equal(c.Data.Key, newCert.Key) {
				logging.Notice("Notify cert %v changed", newCert.Domains)
				c.Data.Fullchain = newCert.Fullchain
				c.Data.Key = newCert.Key
				for _, handleFunc := range c.UpdateHandlers {
					handleFunc(c.Data.Fullchain, c.Data.Key, &c.Config)
				}
			} else {
				logging.Info("Cert %v not changed", newCert.Domains)
			}
		}
	}
}

func MakeCertDXClientDaemon() *CertDXClientDaemon {
	ret := &CertDXClientDaemon{
		Config:    &config.ClientConfigT{},
		ClientOpt: make([]CertDXHttpClientOption, 0),
	}
	ret.Config.SetDefault()
	return ret
}

func (r *CertDXClientDaemon) loadSavedCert(c *config.ClientCertification) (fullchan, key []byte, err error) {
	fullchanPath, keyPath := c.GetFullChainAndKeyPath()
	if utils.FileExists(fullchanPath) && utils.FileExists(keyPath) {
		fullchan, err = os.ReadFile(fullchanPath)
		if err != nil {
			return
		}

		key, err = os.ReadFile(keyPath)
		if err != nil {
			return
		}
	} else {
		err = os.ErrNotExist
	}
	return
}

func (r *CertDXClientDaemon) init() {
	for _, c := range r.Config.Certifications {
		cd := certData{
			Domains: c.Domains,
		}

		fullchan, key, err := r.loadSavedCert(&c)
		if err == nil {
			cd.Fullchain, cd.Key = fullchan, key
		}

		cert := &watchingCert{
			Data:           cd,
			Config:         c,
			UpdateHandlers: []certUpdateHandler{writeCertAndDoCommand},
			UpdateChan:     make(chan certData, 1),
		}
		stop := make(chan struct{})
		cert.Stop.Store(&stop)

		r.certs = append(r.certs, cert)
		go cert.watch(&r.wg)
	}
}

func (r *CertDXClientDaemon) stop() {
	for _, c := range r.certs {
		close(*c.Stop.Load())
	}
}

func (r *CertDXClientDaemon) requestCert(domains []string) *types.HttpCertResp {
	var resp *types.HttpCertResp
	err := utils.Retry(r.Config.Server.RetryCount, func() error {
		certdxClient := MakeCertDXHttpClient(append(r.ClientOpt, WithCertDXServerInfo(&r.Config.Http.MainServer))...)
		var err error
		resp, err = certdxClient.GetCert(domains)
		return err
	})
	if err == nil {
		return resp
	}
	logging.Warn("Failed to get cert %v from MainServer, err: %s", domains, err)

	if r.Config.Http.StandbyServer.Url != "" {
		certdxClient := MakeCertDXHttpClient(append(r.ClientOpt, WithCertDXServerInfo(&r.Config.Http.StandbyServer))...)
		err = utils.Retry(r.Config.Server.RetryCount, func() error {
			var err error
			resp, err = certdxClient.GetCert(domains)
			return err
		})
		if err == nil {
			return resp
		}
		logging.Warn("Failed to get cert %v from StandbyServer, err: %s", domains, err)
	}
	return nil
}

func (r *CertDXClientDaemon) certWatchDog(cert *watchingCert) {
	sleepTime := 1 * time.Hour // default sleep time
	for {
		logging.Info("Requesting cert %v", cert.Config.Domains)
		resp := r.requestCert(cert.Config.Domains)
		if resp != nil {
			if resp.Err != "" {
				logging.Error("Failed to request cert, err: %s", resp.Err)
			} else {
				sleepTime = resp.RenewTimeLeft / 4
				cert.UpdateChan <- certData{
					Domains:   cert.Config.Domains,
					Fullchain: resp.FullChain,
					Key:       resp.Key,
				}
			}
		} else {
			logging.Error("Failed to request cert, retry next round.")
		}
		t := time.After(sleepTime)
		select {
		case <-t:
			// continue
		case <-*cert.Stop.Load():
			return
		}
	}
}

func (r *CertDXClientDaemon) HttpMain() {
	r.init()

	for _, c := range r.certs {
		r.wg.Add(1)
		go func(_c *watchingCert) {
			defer r.wg.Done()
			r.certWatchDog(_c)
		}(c)
	}

	stop := make(chan os.Signal, 1)
	signal.Notify(stop, syscall.SIGINT, syscall.SIGTERM)

	<-stop
	go func() {
		<-stop
		logging.Fatal("Fast dying...")
	}()

	logging.Info("Stopping Http client")
	r.stop()
	r.wg.Wait()
}

func (r *CertDXClientDaemon) GRPCMain() {
	r.init()

	var standByClient *CertDXgRPCClient
	standByExists := r.Config.GRPC.StandbyServer.Server != ""

	mainClient := MakeCertDXgRPCClient(&r.Config.GRPC.MainServer, r.certs)
	if standByExists {
		standByClient = MakeCertDXgRPCClient(&r.Config.GRPC.StandbyServer, r.certs)
	}
	kill := make(chan struct{}, 1)

	r.wg.Add(1)
	go func() {
		defer r.wg.Done()
		retryCount := 0
		for {
			logging.Info("Starting gRPC main stream")
			startTime := time.Now()
			err := mainClient.Stream()
			if err != nil {
				logging.Info("gRPC main stream stopped: %s", err)
				if _, ok := err.(*killed); ok {
					return
				}

				if time.Now().Before(startTime.Add(5 * time.Minute)) {
					retryCount += 1
				} else {
					retryCount = 0
					continue
				}
			}

			logging.Info("Current main server retry count: %d", retryCount)
			if retryCount < r.Config.Server.RetryCount {
				time.Sleep(15 * time.Second)
				continue
			}

			if standByExists && !standByClient.Running.Load() {
				go func() {
					startTime := time.Now()
					retryCount := 0
					for {
						logging.Info("Starting gRPC standby stream")
						err := standByClient.Stream()
						logging.Info("gRPC standby stream stopped: %s", err)
						if _, ok := err.(*killed); ok {
							return
						}
						if time.Now().Before(startTime.Add(5 * time.Minute)) {
							retryCount += 1
						} else {
							retryCount = 0
							continue
						}
						logging.Info("Current standby server retry count: %d", retryCount)
						if retryCount < r.Config.Server.RetryCount {
							time.Sleep(15 * time.Second)
							continue
						}
						retryCount = 0
						logging.Info("Will reconnect standby server after %s", r.Config.Server.ReconnectInterval)
						<-time.After(r.Config.Server.ReconnectDuration)
					}
				}()

				go func() {
					standByClient.Kill <- <-*mainClient.Received.Load()
				}()
			}

			retryCount = 0
			logging.Info("Will reconnect main server after %s", r.Config.Server.ReconnectInterval)
			select {
			case <-time.After(r.Config.Server.ReconnectDuration):
				continue
			case <-kill:
				return
			}
		}
	}()

	stop := make(chan os.Signal, 1)
	signal.Notify(stop, syscall.SIGINT, syscall.SIGTERM)

	<-stop
	go func() {
		<-stop
		logging.Fatal("Fast dying...")
	}()

	logging.Info("Stopping gRPC client")
	r.stop()
	kill <- struct{}{}
	mainClient.Kill <- struct{}{}
	if standByClient != nil {
		standByClient.Kill <- struct{}{}
	}
	r.wg.Wait()
}
