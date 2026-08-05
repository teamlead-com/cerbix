package prober

import (
	"context"
	"net"
	"strings"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	healthpb "google.golang.org/grpc/health/grpc_health_v1"

	"github.com/teamlead-com/cerbix/internal/domain"
)

// grpcProber checks a gRPC server's health (grpc.health.v1) over a plaintext
// connection dialed through the SSRF guard. Success (no conditions) = the health
// service reports SERVING for the overall server (the "" service). The check
// latency is exposed as [RESPONSE_TIME].
type grpcProber struct {
	dial func(ctx context.Context, network, addr string) (net.Conn, error)
}

func (p grpcProber) Probe(ctx context.Context, m domain.Monitor) Result {
	start := time.Now()
	conn, err := grpc.NewClient(grpcTarget(m.Target),
		grpc.WithTransportCredentials(insecure.NewCredentials()),
		grpc.WithContextDialer(func(ctx context.Context, addr string) (net.Conn, error) {
			return p.dial(ctx, "tcp", addr)
		}),
	)
	if err != nil {
		return Result{Connected: false, LatencyMS: elapsedMS(start), Msg: err.Error()}
	}
	defer conn.Close()
	resp, err := healthpb.NewHealthClient(conn).Check(ctx, &healthpb.HealthCheckRequest{Service: ""})
	lat := elapsedMS(start)
	if err != nil {
		return Result{Connected: false, LatencyMS: lat, Msg: err.Error()}
	}
	if resp.GetStatus() != healthpb.HealthCheckResponse_SERVING {
		return Result{Connected: false, LatencyMS: lat, Msg: "not serving: " + resp.GetStatus().String()}
	}
	return Result{Connected: true, LatencyMS: lat}
}

// grpcTarget normalizes a target to host:port, defaulting to the common plaintext
// gRPC port 50051 and tolerating a grpc:// scheme.
func grpcTarget(target string) string {
	target = strings.TrimPrefix(target, "grpc://")
	target = strings.TrimSuffix(target, "/")
	if _, _, err := net.SplitHostPort(target); err != nil {
		return net.JoinHostPort(target, "50051")
	}
	return target
}
