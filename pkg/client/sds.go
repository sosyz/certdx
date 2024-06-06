package client

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"log"
	"os"
	"sync/atomic"
	"time"

	corev3 "github.com/envoyproxy/go-control-plane/envoy/config/core/v3"
	tlsv3 "github.com/envoyproxy/go-control-plane/envoy/extensions/transport_sockets/tls/v3"
	discoveryv3 "github.com/envoyproxy/go-control-plane/envoy/service/discovery/v3"
	secretv3 "github.com/envoyproxy/go-control-plane/envoy/service/secret/v3"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/keepalive"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/anypb"
	"google.golang.org/protobuf/types/known/structpb"
	"pkg.para.party/certdx/pkg/config"
)

const typeUrl = "type.googleapis.com/envoy.extensions.transport_sockets.tls.v3.Secret"
const domainKey = "domains"

type killed struct {
	Err string
}

type respData struct {
	Version string
	Secret  *tlsv3.Secret
}

func (e *killed) Error() string {
	return e.Err
}

type CertDXgRPCClient struct {
	tlsCred credentials.TransportCredentials
	server  *config.ClientGRPCServer
	certs   []*watchingCert

	Kill     chan struct{}
	Running  atomic.Bool
	Received atomic.Pointer[chan struct{}]
}

func MakeCertDXgRPCClient(server *config.ClientGRPCServer, certs []*watchingCert) *CertDXgRPCClient {
	c := &CertDXgRPCClient{
		server: server,
		certs:  certs,
		Kill:   make(chan struct{}, 1),
	}
	received := make(chan struct{})
	c.Received.Store(&received)
	c.Running.Store(false)
	c.tlsCred = credentials.NewTLS(c.getTLSConfig())
	return c
}

func (c *CertDXgRPCClient) getTLSConfig() *tls.Config {
	cert, err := tls.LoadX509KeyPair(c.server.Certificate, c.server.Key)
	if err != nil {
		log.Fatalf("[ERR] Invalid gRPC client cert: %s", err)
	}

	caPEM, err := os.ReadFile(c.server.CA)
	if err != nil {
		log.Fatalf("[ERR] %s", err)
	}

	capool := x509.NewCertPool()
	if !capool.AppendCertsFromPEM(caPEM) {
		log.Fatalf("[ERR] Invalid ca cert")
	}

	return &tls.Config{
		Certificates: []tls.Certificate{cert},
		RootCAs:      capool,
		MinVersion:   tls.VersionTLS13,
		MaxVersion:   tls.VersionTLS13,
	}
}

func (c *CertDXgRPCClient) Stream() error {
	select {
	case <-c.Kill:
		return &killed{Err: "stream killed"}
	default:
	}

	c.Running.Store(true)
	conn, err := grpc.NewClient(c.server.Server,
		grpc.WithTransportCredentials(c.tlsCred),
		grpc.WithKeepaliveParams(keepalive.ClientParameters{
			Time:    30 * time.Second,
			Timeout: 25 * time.Second,
		}),
	)

	if err != nil {
		return fmt.Errorf("new grpc client failed: %w", err)
	}
	defer func() {
		conn.Close()
		c.Running.Store(false)
	}()

	client := secretv3.NewSecretDiscoveryServiceClient(conn)
	stream, err := client.StreamSecrets(context.Background())
	if err != nil {
		return fmt.Errorf("stream secrets failed: %w", err)
	}
	ctx := stream.Context()

	dispatch := map[string]chan respData{}
	ack := make(chan *discoveryv3.DiscoveryRequest)
	errChan := make(chan error)

	for _, cert := range c.certs {
		dispatch[cert.Config.Name] = make(chan respData)
		go c.handleCert(ctx, cert, dispatch[cert.Config.Name], ack, errChan)
	}

	go func() {
		// goroutine for receiving
		for {
			select {
			case <-ctx.Done():
				log.Printf("[INF] Receiving goroutine stopped due to ctx done: %s", ctx.Err())
				return
			default:
			}

			resp, err := stream.Recv()
			if err != nil {
				errChan <- fmt.Errorf("failed receiving request: %w", err)
				return
			}
			newReceived := make(chan struct{})
			close(*c.Received.Swap(&newReceived))

			secretResp := &tlsv3.Secret{}
			err = anypb.UnmarshalTo(resp.Resources[0], secretResp, proto.UnmarshalOptions{})
			if err != nil {
				errChan <- fmt.Errorf("can not unmarshal message from srv: %w", err)
				return
			}

			respChan, ok := dispatch[secretResp.Name]
			if !ok {
				errChan <- fmt.Errorf("unexcepted cert: %s", secretResp.Name)
				return
			}

			respChan <- respData{
				Version: resp.VersionInfo,
				Secret:  secretResp,
			}
		}
	}()

	go func() {
		// goroutine for sending
		domainSets := map[string]interface{}{}
		resourceNames := []string{}
		for _, cert := range c.certs {
			_domainSet := []interface{}{}
			for _, domain := range cert.Config.Domains {
				_domainSet = append(_domainSet, domain)
			}
			domainSets[cert.Config.Name] = _domainSet
			resourceNames = append(resourceNames, cert.Config.Name)
		}

		metaDataStruct, err := structpb.NewStruct(map[string]interface{}{
			domainKey: domainSets,
		})
		if err != nil {
			errChan <- fmt.Errorf("failed constructing meta data struct: %w", err)
			return
		}

		packReq := &discoveryv3.DiscoveryRequest{
			TypeUrl:       typeUrl,
			ResourceNames: resourceNames,
			Node: &corev3.Node{
				Metadata: metaDataStruct,
			},
		}

		err = stream.Send(packReq)
		if err != nil {
			errChan <- fmt.Errorf("failed sending request: %w", err)
			return
		}

		for {
			select {
			case a := <-ack:
				if err := stream.Send(a); err != nil {
					// a failed in sending should make the context fail as well.
					errChan <- fmt.Errorf("failed sending ack: %w", err)
					return
				}
			case <-ctx.Done():
				log.Printf("[INF] Message sender stopped due to ctx done: %s", ctx.Err())
				return
			}
		}
	}()

	select {
	case <-ctx.Done():
		log.Printf("[INF] Stream end due to ctx Done: %s", ctx.Err())
		return ctx.Err()
	case err := <-errChan:
		log.Printf("[ERR] Stream end due to errored: %s", err)
		return err
	case <-c.Kill:
		log.Printf("[INF] Stream end due to explicit kill.")
		return &killed{Err: "stream killed"}
	}
}

func (c *CertDXgRPCClient) handleCert(ctx context.Context, cert *watchingCert,
	resp chan respData, ack chan *discoveryv3.DiscoveryRequest, errChan chan error) {

	for {
		select {
		case _respData := <-resp:
			respCert, ok := _respData.Secret.Type.(*tlsv3.Secret_TlsCertificate)
			if !ok {
				errChan <- fmt.Errorf("unexcepted resp type")
				return
			}

			cert.UpdateChan <- certData{
				Domains:   cert.Config.Domains,
				Fullchain: respCert.TlsCertificate.CertificateChain.GetInlineBytes(),
				Key:       respCert.TlsCertificate.PrivateKey.GetInlineBytes(),
			}

			ack <- &discoveryv3.DiscoveryRequest{
				TypeUrl:       typeUrl,
				VersionInfo:   _respData.Version,
				ResourceNames: []string{cert.Config.Name},
			}
		case <-ctx.Done():
			log.Printf("[ERR] handler stopped due to ctx done: %s", ctx.Err())
			return
		}
	}
}
