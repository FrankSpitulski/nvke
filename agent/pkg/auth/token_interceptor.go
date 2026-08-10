package auth

import (
	"context"
	"crypto/subtle"
	"fmt"
	"log/slog"
	"os"
	"strings"
	"sync"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
)

type TokenAuthenticator struct {
	mu        sync.RWMutex
	token     string
	tokenFile string
}

func NewTokenAuthenticator(tokenFile string) (*TokenAuthenticator, error) {
	a := &TokenAuthenticator{tokenFile: tokenFile}
	if err := a.loadToken(); err != nil {
		fmt.Printf("could not read token: %v\n", err)
		return nil, err
	}
	go a.watchToken()
	return a, nil
}

func (a *TokenAuthenticator) loadToken() error {
	b, err := os.ReadFile(a.tokenFile)
	if err != nil {
		fmt.Printf("could not read token: %v\n", err)
		return err
	}
	t := strings.TrimSpace(string(b))
	a.mu.Lock()
	a.token = t
	a.mu.Unlock()
	return nil
}

func (a *TokenAuthenticator) watchToken() {
	ticker := time.NewTicker(10 * time.Second)
	defer ticker.Stop()
	for range ticker.C {
		if err := a.loadToken(); err != nil {
			slog.Error("failed to reload auth token", "err", err)
		}
	}
}

func (a *TokenAuthenticator) UnaryInterceptor() grpc.UnaryServerInterceptor {
	return func(ctx context.Context, req any, info *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (any, error) {
		md, ok := metadata.FromIncomingContext(ctx)
		if !ok {
			return nil, status.Error(codes.Unauthenticated, "missing request metadata")
		}
		ah := md.Get("authorization")
		if len(ah) == 0 {
			return nil, status.Error(codes.Unauthenticated, "missing authorization header")
		}
		token := strings.TrimPrefix(ah[0], "Bearer ")
		a.mu.RLock()
		expected := a.token
		a.mu.RUnlock()
		if subtle.ConstantTimeCompare([]byte(token), []byte(expected)) != 1 {
			return nil, status.Error(codes.Unauthenticated, "unauthorized")
		}
		return handler(ctx, req)
	}
}
