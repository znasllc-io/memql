package releasecatalog

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
	"io"
	"math/big"
	"net"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/znasllc-io/memql/component/auth"
	pb "github.com/znasllc-io/memql/component/grpc/gen"
	pl "github.com/znasllc-io/memql/component/pipelines"
	"github.com/znasllc-io/memql/sdk/go/client"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/structpb"
)

type publisherFixture struct {
	pb.UnimplementedMemqlServiceServer
	mu          sync.Mutex
	page, item  map[string]any
	block       bool
	privateFail bool
	streams     atomic.Int64
	queries     atomic.Int64
	closed      atomic.Int64
}

func (f *publisherFixture) Stream(stream grpc.BidiStreamingServer[pb.MemqlClientMessage, pb.MemqlServerMessage]) error {
	f.streams.Add(1)
	defer f.closed.Add(1)
	md, _ := metadata.FromIncomingContext(stream.Context())
	if strings.Join(md.Get("authorization"), "") != "Bearer fixture-private-token" {
		return status.Error(codes.Unauthenticated, "fixture-private-token was not provided")
	}
	for {
		message, err := stream.Recv()
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return err
		}
		response := &pb.MemqlServerMessage{CorrelateTo: message.GetMessageId()}
		if message.GetClientHello() != nil {
			response.Payload = &pb.MemqlServerMessage_ServerHello{ServerHello: &pb.ServerHello{NodeId: "publisher", Version: "v1"}}
		} else if query := message.GetExecuteQuery(); query != nil {
			f.queries.Add(1)
			f.mu.Lock()
			block, fail := f.block, f.privateFail
			body := f.item
			if strings.HasPrefix(query.Query, "builtin releaseListPublishedCandidates(") {
				body = f.page
			} else if !strings.HasPrefix(query.Query, "builtin releaseGetPublishedCandidate(") {
				f.mu.Unlock()
				return status.Error(codes.InvalidArgument, "unexpected construct")
			}
			payload, err := structpb.NewStruct(body)
			f.mu.Unlock()
			if err != nil {
				return err
			}
			if fail {
				return status.Error(codes.Internal, "private publisher diagnostic fixture-private-token")
			}
			if block {
				<-stream.Context().Done()
				return stream.Context().Err()
			}
			response.Payload = &pb.MemqlServerMessage_QueryResult{QueryResult: &pb.QueryResultChunk{Done: true, Result: &pb.Result{Bundle: &pb.GraphBundle{Nodes: []*pb.MemoryNode{{Id: "release:result", Concept: "integration:release:result", Payload: payload}}}}}}
		} else {
			continue
		}
		if err := stream.Send(response); err != nil {
			return err
		}
	}
}

func readerContext() context.Context {
	return auth.ContextWithAccess(context.Background(), &auth.AccessContext{UserId: "operator", Role: auth.RoleDeveloper})
}

func fixtureRelease() pl.PublishedRelease {
	d := "sha256:" + strings.Repeat("a", 64)
	return pl.PublishedRelease{FormatVersion: 1, Publisher: "fixture", CandidateID: d, ApprovalID: "approval", WorkflowDigest: d, ProvenanceDigest: d, Compatibility: []pl.ReleaseCompatibility{}, Components: []pl.PublishedComponent{{Name: "engine", Version: "0.25.0", Repository: "acme/engine", Commit: strings.Repeat("b", 40), Artifacts: []pl.PublishedArtifact{{Name: "image", Kind: "oci", Platform: "linux/arm64", Digest: d, Size: 100, ImageDigest: d, Locations: []pl.PublishedLocation{{Kind: "oci", Origin: "https://registry.example", Repository: "acme/engine"}}}}}}}
}

func newFixture(t *testing.T) (*publisherFixture, source, string, string) {
	t.Helper()
	pub, key, err := ed25519.GenerateKey(rand.Reader)
	require.NoError(t, err)
	release := fixtureRelease()
	body, err := pl.SignPublishedRelease(release, "key", key)
	require.NoError(t, err)
	verified, err := pl.VerifyPublishedRelease(body, "fixture", map[string]ed25519.PublicKey{"key": pub})
	require.NoError(t, err)
	var envelope map[string]any
	require.NoError(t, json.Unmarshal(body, &envelope))
	f := &publisherFixture{
		page: map[string]any{"releases": []any{map[string]any{"candidateId": release.CandidateID, "catalogDigest": verified.Digest(), "components": []any{map[string]any{"name": "unsigned-list-spoof", "version": "9.9.9", "repository": "untrusted/name", "commit": "untrusted"}}}}, "nextCursor": "publisher-cursor"},
		item: map[string]any{"candidateId": release.CandidateID, "catalogDigest": verified.Digest(), "envelope": envelope, "release": map[string]any{"publisher": "unsigned-projection-spoof"}},
	}
	tlsPub, tlsKey, err := ed25519.GenerateKey(rand.Reader)
	require.NoError(t, err)
	cert := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "catalog fixture"}, NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour), IPAddresses: []net.IP{net.ParseIP("127.0.0.1")}, IsCA: true, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageDigitalSignature | x509.KeyUsageCertSign, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}}
	der, err := x509.CreateCertificate(rand.Reader, cert, cert, tlsPub, tlsKey)
	require.NoError(t, err)
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	server := grpc.NewServer(grpc.Creds(credentials.NewTLS(&tls.Config{MinVersion: tls.VersionTLS12, Certificates: []tls.Certificate{{Certificate: [][]byte{der}, PrivateKey: tlsKey}}})))
	pb.RegisterMemqlServiceServer(server, f)
	go func() { _ = server.Serve(listener) }()
	t.Cleanup(server.Stop)
	s := source{ID: "publisher", Publisher: "fixture", Endpoint: "https://" + listener.Addr().String(), CredentialSecret: "PUBLISHER_TOKEN", PublicKeys: map[string]string{"key": base64.StdEncoding.EncodeToString(pub)}, RootCAPEM: string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}))}
	return f, s, release.CandidateID, verified.Digest()
}

func fixtureReader(t *testing.T, s *source) *Reader {
	t.Helper()
	return New(func(_ context.Context, key string) (string, error) {
		require.Equal(t, ConfigurationVariable, key)
		body, err := json.Marshal(configuration{FormatVersion: 1, Sources: []source{*s}})
		return string(body), err
	}, func(_ context.Context, key string) (string, error) {
		require.Equal(t, "PUBLISHER_TOKEN", key)
		return "fixture-private-token", nil
	})
}

func TestDiscoveryUsesRealTLSStreamAndIndependentVerifiedEvidence(t *testing.T) {
	f, s, candidate, digest := newFixture(t)
	a := fixtureReader(t, &s)
	page, err := a.List(readerContext(), s.ID, "", 1)
	require.NoError(t, err)
	require.Equal(t, "publisher-cursor", page.NextCursor)
	require.Equal(t, "engine", page.Releases[0].Components[0].Name)
	require.Equal(t, "0.25.0", page.Releases[0].Components[0].Version)
	// Another receiving replica has none of the first reader's local state.
	b := fixtureReader(t, &s)
	verified, err := b.Get(readerContext(), s.ID, candidate, digest)
	require.NoError(t, err)
	release, err := verified.Release()
	require.NoError(t, err)
	require.Equal(t, "fixture", release.Publisher)
	require.Equal(t, digest, verified.Digest())
	release.Components[0].Version = "mutated"
	again, err := verified.Release()
	require.NoError(t, err)
	require.Equal(t, "0.25.0", again.Components[0].Version)
	require.EqualValues(t, 3, f.queries.Load())
	require.Eventually(t, func() bool { return f.closed.Load() == 2 }, time.Second, time.Millisecond)
	// Configuration is read again; old verified discovery is not cached trust.
	other, _, _ := ed25519.GenerateKey(rand.Reader)
	s.PublicKeys["key"] = base64.StdEncoding.EncodeToString(other)
	_, err = b.Get(readerContext(), s.ID, candidate, digest)
	require.ErrorContains(t, err, "configured publisher keys")
}

func TestConnectedSDKStreamOutlivesItsConnectDeadline(t *testing.T) {
	f, s, candidate, _ := newFixture(t)
	_, roots, err := s.trust()
	require.NoError(t, err)
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	conn, err := client.Connect(ctx, client.ConnectConfig{Endpoint: s.Endpoint, Token: "fixture-private-token", RootCAs: roots})
	require.NoError(t, err)
	defer conn.Close()
	cancel()
	_, err = client.NewQueryClient(conn.Dispatcher()).ReleaseGetPublishedCandidate(t.Context(), client.ReleaseGetPublishedCandidateArgs{CandidateId: candidate})
	require.NoError(t, err)
	require.EqualValues(t, 1, f.queries.Load())
}

func TestDiscoveryRefusesSubstitutionAndMalformedPages(t *testing.T) {
	for _, fault := range []string{"signature", "publisher", "candidate", "digest", "page bound", "duplicate", "cursor", "missing envelope", "oversized wire", "private diagnostic"} {
		t.Run(fault, func(t *testing.T) {
			f, s, _, _ := newFixture(t)
			switch fault {
			case "signature":
				f.item["envelope"].(map[string]any)["payload"] = base64.StdEncoding.EncodeToString([]byte("tampered"))
			case "publisher":
				s.Publisher = "other"
			case "candidate":
				f.item["candidateId"] = "sha256:" + strings.Repeat("c", 64)
			case "digest":
				f.item["catalogDigest"] = "sha256:" + strings.Repeat("c", 64)
			case "page bound", "duplicate":
				f.page["releases"] = append(f.page["releases"].([]any), f.page["releases"].([]any)[0])
			case "cursor":
				f.page["nextCursor"] = strings.Repeat("a", 1025)
			case "missing envelope":
				delete(f.item, "envelope")
			case "oversized wire":
				f.page["extra"] = strings.Repeat("x", 5<<20)
			case "private diagnostic":
				f.privateFail = true
			}
			limit := 1
			if fault == "duplicate" {
				limit = 2
			}
			page, err := fixtureReader(t, &s).List(readerContext(), s.ID, "", limit)
			require.Error(t, err)
			require.Empty(t, page.Releases)
			require.NotContains(t, err.Error(), "fixture-private-token")
			require.NotContains(t, err.Error(), "private publisher diagnostic")
		})
	}
}

func TestDiscoveryAuthorizationPrecedesConfigurationAndCredentials(t *testing.T) {
	for _, access := range []*auth.AccessContext{nil, {UserId: "operator", Role: auth.RoleReader}, {UserId: "operator", Role: auth.RoleWriter}, {Role: auth.RoleOwner}, {UserId: "operator", Role: auth.RoleOwner, IsAnonymous: true}, {UserId: "operator", Role: auth.RoleOwner, Synthetic: true}, {UserId: "operator", Role: auth.RoleOwner, RoleStandIn: true}} {
		ctx := context.Background()
		if access != nil {
			ctx = auth.ContextWithAccess(ctx, access)
		}
		forbidden := func(context.Context, string) (string, error) {
			t.Fatal("unauthorized call reached configuration or credentials")
			return "", nil
		}
		r := New(forbidden, forbidden)
		_, err := r.Sources(ctx)
		require.Error(t, err)
		_, err = r.List(ctx, "publisher", "", 1)
		require.Error(t, err)
		_, err = r.Get(ctx, "publisher", "sha256:"+strings.Repeat("a", 64), "sha256:"+strings.Repeat("b", 64))
		require.Error(t, err)
	}
}

func TestDiscoveryClosesCancelledStreamAndVerifiesTransportTrust(t *testing.T) {
	for _, fault := range []string{"untrusted certificate", "wrong hostname"} {
		t.Run(fault, func(t *testing.T) {
			f, s, candidate, digest := newFixture(t)
			if fault == "untrusted certificate" {
				s.RootCAPEM = ""
			}
			if fault == "wrong hostname" {
				s.Endpoint = strings.Replace(s.Endpoint, "127.0.0.1", "localhost", 1)
			}
			ctx, cancel := context.WithTimeout(readerContext(), 200*time.Millisecond)
			defer cancel()
			_, err := fixtureReader(t, &s).Get(ctx, s.ID, candidate, digest)
			require.Error(t, err)
			require.Zero(t, f.queries.Load())
		})
	}
	f, s, candidate, digest := newFixture(t)
	f.block = true
	ctx, cancel := context.WithTimeout(readerContext(), 3*time.Second)
	defer cancel()
	r := fixtureReader(t, &s)
	done := make(chan error, 1)
	go func() {
		_, err := r.Get(ctx, s.ID, candidate, digest)
		done <- err
	}()
	require.Eventually(t, func() bool { return f.queries.Load() == 1 }, time.Second, time.Millisecond)
	cancel()
	select {
	case err := <-done:
		require.ErrorIs(t, err, context.Canceled)
	case <-time.After(time.Second):
		t.Fatal("cancelled publisher read did not return")
	}
	require.Eventually(t, func() bool { return f.closed.Load() == 1 }, time.Second, time.Millisecond)
}
