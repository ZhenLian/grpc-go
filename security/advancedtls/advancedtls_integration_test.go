/*
 *
 * Copyright 2020 gRPC authors.
 *
 * Licensed under the Apache License, Version 2.0 (the "License");
 * you may not use this file except in compliance with the License.
 * You may obtain a copy of the License at
 *
 *     http://www.apache.org/licenses/LICENSE-2.0
 *
 * Unless required by applicable law or agreed to in writing, software
 * distributed under the License is distributed on an "AS IS" BASIS,
 * WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
 * See the License for the specific language governing permissions and
 * limitations under the License.
 *
 */

package advancedtls

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"net"
	"sync"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
	pb "google.golang.org/grpc/examples/helloworld/helloworld"
	"google.golang.org/grpc/security/advancedtls/internal/testutils"
)

var (
	address = "localhost:50051"
	port    = ":50051"
)

// stageInfo contains a stage number indicating the current phase of each
// integration test, and a mutex.
// Based on the stage number of current test, we will use different
// certificates and custom verification functions to check if our tests behave
// as expected.
type stageInfo struct {
	mutex sync.Mutex
	stage int
}

func (s *stageInfo) increase() {
	s.mutex.Lock()
	defer s.mutex.Unlock()
	s.stage = s.stage + 1
}

func (s *stageInfo) read() int {
	s.mutex.Lock()
	defer s.mutex.Unlock()
	return s.stage
}

func (s *stageInfo) reset() {
	s.mutex.Lock()
	defer s.mutex.Unlock()
	s.stage = 0
}

type greeterServer struct {
	pb.UnimplementedGreeterServer
}

// sayHello is a simple implementation of the pb.GreeterServer SayHello method.
func (greeterServer) SayHello(ctx context.Context, in *pb.HelloRequest) (*pb.HelloReply, error) {
	return &pb.HelloReply{Message: "Hello " + in.Name}, nil
}

func callAndVerify(msg string, client pb.GreeterClient, shouldFail bool) error {
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	_, err := client.SayHello(ctx, &pb.HelloRequest{Name: msg})
	if want, got := shouldFail == true, err != nil; got != want {
		return fmt.Errorf("want and got mismatch,  want shouldFail=%v, got fail=%v, rpc error: %v", want, got, err)
	}
	return nil
}

func callAndVerifyWithClientConn(connCtx context.Context, msg string, creds credentials.TransportCredentials, shouldFail bool) (*grpc.ClientConn, pb.GreeterClient, error) {
	var conn *grpc.ClientConn
	var err error
	// If we want the test to fail, we establish a non-blocking connection to
	// avoid it hangs and killed by the context.
	if shouldFail {
		conn, err = grpc.DialContext(connCtx, address, grpc.WithTransportCredentials(creds))
		if err != nil {
			return nil, nil, fmt.Errorf("client failed to connect to %s. Error: %v", address, err)
		}
	} else {
		conn, err = grpc.DialContext(connCtx, address, grpc.WithTransportCredentials(creds), grpc.WithBlock())
		if err != nil {
			return nil, nil, fmt.Errorf("client failed to connect to %s. Error: %v", address, err)
		}
	}
	greetClient := pb.NewGreeterClient(conn)
	err = callAndVerify(msg, greetClient, shouldFail)
	if err != nil {
		return nil, nil, err
	}
	return conn, greetClient, nil
}

// The advanced TLS features are tested in different stages.
// At stage 0, we establish a good connection between client and server.
// At stage 1, we change one factor(it could be we change the server's
// certificate, or custom verification function, etc), and test if the
// following connections would be dropped.
// At stage 2, we re-establish the connection by changing the counterpart of
// the factor we modified in stage 1.
// (could be change the client's trust certificate, or change custom
// verification function, etc)
func (s) TestEnd2End(t *testing.T) {
	cs := &testutils.CertStore{}
	if err := cs.LoadCerts(); err != nil {
		t.Fatalf("cs.LoadCerts() failed, err: %v", err)
	}
	stage := &stageInfo{}
	for _, test := range []struct {
		desc             string
		clientCert       []tls.Certificate
		clientGetCert    func(*tls.CertificateRequestInfo) (*tls.Certificate, error)
		clientRoot       *x509.CertPool
		clientGetRoot    func(params *GetRootCAsParams) (*GetRootCAsResults, error)
		clientVerifyFunc CustomVerificationFunc
		clientVType      VerificationType
		serverCert       []tls.Certificate
		serverGetCert    func(*tls.ClientHelloInfo) ([]*tls.Certificate, error)
		serverRoot       *x509.CertPool
		serverGetRoot    func(params *GetRootCAsParams) (*GetRootCAsResults, error)
		serverVerifyFunc CustomVerificationFunc
		serverVType      VerificationType
	}{
		// Test Scenarios:
		// At initialization(stage = 0), client will be initialized with cert
		// ClientCert1 and ClientTrust1, server with ServerCert1 and ServerTrust1.
		// The mutual authentication works at the beginning, since ClientCert1 is
		// trusted by ServerTrust1, and ServerCert1 by ClientTrust1.
		// At stage 1, client changes ClientCert1 to ClientCert2. Since ClientCert2
		// is not trusted by ServerTrust1, following rpc calls are expected to
		// fail, while the previous rpc calls are still good because those are
		// already authenticated.
		// At stage 2, the server changes ServerTrust1 to ServerTrust2, and we
		// should see it again accepts the connection, since ClientCert2 is trusted
		// by ServerTrust2.
		{
			desc: "TestClientPeerCertReloadServerTrustCertReload",
			clientGetCert: func(*tls.CertificateRequestInfo) (*tls.Certificate, error) {
				switch stage.read() {
				case 0:
					return &cs.ClientCert1, nil
				default:
					return &cs.ClientCert2, nil
				}
			},
			clientRoot: cs.ClientTrust1,
			clientVerifyFunc: func(params *VerificationFuncParams) (*VerificationResults, error) {
				return &VerificationResults{}, nil
			},
			clientVType: CertVerification,
			serverCert:  []tls.Certificate{cs.ServerCert1},
			serverGetRoot: func(params *GetRootCAsParams) (*GetRootCAsResults, error) {
				switch stage.read() {
				case 0, 1:
					return &GetRootCAsResults{TrustCerts: cs.ServerTrust1}, nil
				default:
					return &GetRootCAsResults{TrustCerts: cs.ServerTrust2}, nil
				}
			},
			serverVerifyFunc: func(params *VerificationFuncParams) (*VerificationResults, error) {
				return &VerificationResults{}, nil
			},
			serverVType: CertVerification,
		},
		// Test Scenarios:
		// At initialization(stage = 0), client will be initialized with cert
		// ClientCert1 and ClientTrust1, server with ServerCert1 and ServerTrust1.
		// The mutual authentication works at the beginning, since ClientCert1 is
		// trusted by ServerTrust1, and ServerCert1 by ClientTrust1.
		// At stage 1, server changes ServerCert1 to ServerCert2. Since ServerCert2
		// is not trusted by ClientTrust1, following rpc calls are expected to
		// fail, while the previous rpc calls are still good because those are
		// already authenticated.
		// At stage 2, the client changes ClientTrust1 to ClientTrust2, and we
		// should see it again accepts the connection, since ServerCert2 is trusted
		// by ClientTrust2.
		{
			desc:       "TestServerPeerCertReloadClientTrustCertReload",
			clientCert: []tls.Certificate{cs.ClientCert1},
			clientGetRoot: func(params *GetRootCAsParams) (*GetRootCAsResults, error) {
				switch stage.read() {
				case 0, 1:
					return &GetRootCAsResults{TrustCerts: cs.ClientTrust1}, nil
				default:
					return &GetRootCAsResults{TrustCerts: cs.ClientTrust2}, nil
				}
			},
			clientVerifyFunc: func(params *VerificationFuncParams) (*VerificationResults, error) {
				return &VerificationResults{}, nil
			},
			clientVType: CertVerification,
			serverGetCert: func(*tls.ClientHelloInfo) ([]*tls.Certificate, error) {
				switch stage.read() {
				case 0:
					return []*tls.Certificate{&cs.ServerCert1}, nil
				default:
					return []*tls.Certificate{&cs.ServerCert2}, nil
				}
			},
			serverRoot: cs.ServerTrust1,
			serverVerifyFunc: func(params *VerificationFuncParams) (*VerificationResults, error) {
				return &VerificationResults{}, nil
			},
			serverVType: CertVerification,
		},
		// Test Scenarios:
		// At initialization(stage = 0), client will be initialized with cert
		// ClientCert1 and ClientTrust1, server with ServerCert1 and ServerTrust1.
		// The mutual authentication works at the beginning, since ClientCert1
		// trusted by ServerTrust1, ServerCert1 by ClientTrust1, and also the
		// custom verification check allows the CommonName on ServerCert1.
		// At stage 1, server changes ServerCert1 to ServerCert2, and client
		// changes ClientTrust1 to ClientTrust2. Although ServerCert2 is trusted by
		// ClientTrust2, our authorization check only accepts ServerCert1, and
		// hence the following calls should fail. Previous connections should
		// not be affected.
		// At stage 2, the client changes authorization check to only accept
		// ServerCert2. Now we should see the connection becomes normal again.
		{
			desc:       "TestClientCustomVerification",
			clientCert: []tls.Certificate{cs.ClientCert1},
			clientGetRoot: func(params *GetRootCAsParams) (*GetRootCAsResults, error) {
				switch stage.read() {
				case 0:
					return &GetRootCAsResults{TrustCerts: cs.ClientTrust1}, nil
				default:
					return &GetRootCAsResults{TrustCerts: cs.ClientTrust2}, nil
				}
			},
			clientVerifyFunc: func(params *VerificationFuncParams) (*VerificationResults, error) {
				if len(params.RawCerts) == 0 {
					return nil, fmt.Errorf("no peer certs")
				}
				cert, err := x509.ParseCertificate(params.RawCerts[0])
				if err != nil || cert == nil {
					return nil, fmt.Errorf("failed to parse certificate: " + err.Error())
				}
				authzCheck := false
				switch stage.read() {
				case 0, 1:
					// foo.bar.com is the common name on ServerCert1
					if cert.Subject.CommonName == "foo.bar.com" {
						authzCheck = true
					}
				default:
					// foo.bar.server2.com is the common name on ServerCert2
					if cert.Subject.CommonName == "foo.bar.server2.com" {
						authzCheck = true
					}
				}
				if authzCheck {
					return &VerificationResults{}, nil
				}
				return nil, fmt.Errorf("custom authz check fails")
			},
			clientVType: CertVerification,
			serverGetCert: func(*tls.ClientHelloInfo) ([]*tls.Certificate, error) {
				switch stage.read() {
				case 0:
					return []*tls.Certificate{&cs.ServerCert1}, nil
				default:
					return []*tls.Certificate{&cs.ServerCert2}, nil
				}
			},
			serverRoot: cs.ServerTrust1,
			serverVerifyFunc: func(params *VerificationFuncParams) (*VerificationResults, error) {
				return &VerificationResults{}, nil
			},
			serverVType: CertVerification,
		},
		// Test Scenarios:
		// At initialization(stage = 0), client will be initialized with cert
		// ClientCert1 and ClientTrust1, server with ServerCert1 and ServerTrust1.
		// The mutual authentication works at the beginning, since ClientCert1
		// trusted by ServerTrust1, ServerCert1 by ClientTrust1, and also the
		// custom verification check on server side allows all connections.
		// At stage 1, server disallows the the connections by setting custom
		// verification check. The following calls should fail. Previous
		// connections should not be affected.
		// At stage 2, server allows all the connections again and the
		// authentications should go back to normal.
		{
			desc:       "TestServerCustomVerification",
			clientCert: []tls.Certificate{cs.ClientCert1},
			clientRoot: cs.ClientTrust1,
			clientVerifyFunc: func(params *VerificationFuncParams) (*VerificationResults, error) {
				return &VerificationResults{}, nil
			},
			clientVType: CertVerification,
			serverCert:  []tls.Certificate{cs.ServerCert1},
			serverRoot:  cs.ServerTrust1,
			serverVerifyFunc: func(params *VerificationFuncParams) (*VerificationResults, error) {
				switch stage.read() {
				case 0, 2:
					return &VerificationResults{}, nil
				case 1:
					return nil, fmt.Errorf("custom authz check fails")
				default:
					return nil, fmt.Errorf("custom authz check fails")
				}
			},
			serverVType: CertVerification,
		},
	} {
		test := test
		t.Run(test.desc, func(t *testing.T) {
			// Start a server using ServerOptions in another goroutine.
			serverOptions := &ServerOptions{
				IdentityOptions: IdentityCertificateOptions{
					Certificates:                     test.serverCert,
					GetIdentityCertificatesForServer: test.serverGetCert,
				},
				RootOptions: RootCertificateOptions{
					RootCACerts:         test.serverRoot,
					GetRootCertificates: test.serverGetRoot,
				},
				RequireClientCert: true,
				VerifyPeer:        test.serverVerifyFunc,
				VType:             test.serverVType,
			}
			serverTLSCreds, err := NewServerCreds(serverOptions)
			if err != nil {
				t.Fatalf("failed to create server creds: %v", err)
			}
			s := grpc.NewServer(grpc.Creds(serverTLSCreds))
			defer s.Stop()
			lis, err := net.Listen("tcp", port)
			if err != nil {
				t.Fatalf("failed to listen: %v", err)
			}
			defer lis.Close()
			pb.RegisterGreeterServer(s, greeterServer{})
			go s.Serve(lis)
			clientOptions := &ClientOptions{
				IdentityOptions: IdentityCertificateOptions{
					Certificates:                     test.clientCert,
					GetIdentityCertificatesForClient: test.clientGetCert,
				},
				VerifyPeer: test.clientVerifyFunc,
				RootOptions: RootCertificateOptions{
					RootCACerts:         test.clientRoot,
					GetRootCertificates: test.clientGetRoot,
				},
				VType: test.clientVType,
			}
			clientTLSCreds, err := NewClientCreds(clientOptions)
			if err != nil {
				t.Fatalf("clientTLSCreds failed to create")
			}
			// ------------------------Scenario 1------------------------------------
			// stage = 0, initial connection should succeed
			ctx1, cancel1 := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel1()
			conn, greetClient, err := callAndVerifyWithClientConn(ctx1, "rpc call 1", clientTLSCreds, false)
			if err != nil {
				t.Fatal(err)
			}
			defer conn.Close()
			// ----------------------------------------------------------------------
			stage.increase()
			// ------------------------Scenario 2------------------------------------
			// stage = 1, previous connection should still succeed
			err = callAndVerify("rpc call 2", greetClient, false)
			if err != nil {
				t.Fatal(err)
			}
			// ------------------------Scenario 3------------------------------------
			// stage = 1, new connection should fail
			ctx2, cancel2 := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel2()
			conn2, greetClient, err := callAndVerifyWithClientConn(ctx2, "rpc call 3", clientTLSCreds, true)
			if err != nil {
				t.Fatal(err)
			}
			defer conn2.Close()
			// ----------------------------------------------------------------------
			stage.increase()
			// ------------------------Scenario 4------------------------------------
			// stage = 2,  new connection should succeed
			ctx3, cancel3 := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel3()
			conn3, greetClient, err := callAndVerifyWithClientConn(ctx3, "rpc call 4", clientTLSCreds, false)
			if err != nil {
				t.Fatal(err)
			}
			defer conn3.Close()
			// ----------------------------------------------------------------------
			stage.reset()
		})
	}
}
