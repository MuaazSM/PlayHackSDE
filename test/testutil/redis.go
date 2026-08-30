package testutil

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/redis/go-redis/v9"
	"github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/wait"
)

var (
	redisOnce sync.Once
	redisInst *redis.Client
	redisAddr string
	redisErr  error
)

// Redis returns a live Redis client, flushed clean.
//
// Same lifecycle as Postgres: one container per package run, reused, reset on
// every call. Redis is never authoritative here — it holds cache, rate-limit
// counters and pub/sub — so a wipe between tests costs nothing.
func Redis(t *testing.T) *redis.Client {
	t.Helper()

	redisOnce.Do(func() { redisInst, redisAddr, redisErr = startRedis() })
	if redisErr != nil {
		t.Fatalf("testutil: redis: %v", redisErr)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := redisInst.FlushAll(ctx).Err(); err != nil {
		t.Fatalf("testutil: redis flush: %v", err)
	}
	return redisInst
}

// RedisAddr is the host:port of the test Redis, for code that builds its own
// client.
func RedisAddr(t *testing.T) string {
	t.Helper()
	Redis(t)
	return redisAddr
}

func startRedis() (*redis.Client, string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()

	container, err := testcontainers.GenericContainer(ctx, testcontainers.GenericContainerRequest{
		ContainerRequest: testcontainers.ContainerRequest{
			Image:        "redis:7-alpine",
			ExposedPorts: []string{"6379/tcp"},
			WaitingFor:   wait.ForLog("Ready to accept connections").WithStartupTimeout(60 * time.Second),
		},
		Started: true,
	})
	if err != nil {
		return nil, "", fmt.Errorf("starting container: %w", err)
	}

	host, err := container.Host(ctx)
	if err != nil {
		return nil, "", err
	}
	port, err := container.MappedPort(ctx, "6379/tcp")
	if err != nil {
		return nil, "", err
	}

	addr := fmt.Sprintf("%s:%s", host, port.Port())
	client := redis.NewClient(&redis.Options{Addr: addr})
	if err := client.Ping(ctx).Err(); err != nil {
		return nil, "", fmt.Errorf("ping: %w", err)
	}
	return client, addr, nil
}

// DeadRedis returns a client pointed at an address nothing is listening on,
// which is what a Redis outage looks like from inside the service.
func DeadRedis() *redis.Client {
	return redis.NewClient(&redis.Options{
		Addr:        "127.0.0.1:1",
		DialTimeout: 50 * time.Millisecond,
		MaxRetries:  -1,
	})
}
