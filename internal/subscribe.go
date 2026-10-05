package internal

import (
	"context"
	"log/slog"
	"os"
	"time"

	"github.com/Muxcore-Media/contracts-media/events"
	"github.com/Muxcore-Media/core/sdk/go/client"
)

func (m *Module) consumeUserDeleted(ctx context.Context) {
	c := m.dialCore(ctx)
	if c == nil {
		return
	}
	ch, cancel, err := c.Events.Subscribe(ctx, events.EventIdentityUserDeleted)
	if err != nil {
		slog.Warn("subscribe identity.user.deleted", "error", err)
		return
	}
	defer cancel()
	slog.Info("subscribed to identity.user.deleted")
	for {
		select {
		case <-ctx.Done():
			return
		case ev, ok := <-ch:
			if !ok {
				return
			}
			if ev == nil {
				continue
			}
			if err := ApplyUserDeleted(m.store, ev.Payload); err != nil {
				slog.Warn("apply identity.user.deleted", "error", err)
			}
		}
	}
}

func (m *Module) dialCore(ctx context.Context) *client.Client {
	meshAddr := os.Getenv("MUXCORE_GRPC_ADDR")
	if meshAddr == "" {
		meshAddr = "localhost:9090"
	}
	var opts []client.Option
	if os.Getenv("MUXCORE_INSECURE_DISABLE_TLS") == "true" || os.Getenv("MUXCORE_GRPC_INSECURE") == "true" {
		opts = append(opts, client.WithInsecure())
	}
	backoff := 100 * time.Millisecond
	for {
		c, err := client.Dial(meshAddr, opts...)
		if err == nil {
			if ctx.Err() != nil {
				c.Close()
				return nil
			}
			m.mc.Store(c)
			slog.Info("userdata-local connected to core", "addr", meshAddr)
			return c
		}
		slog.Error("userdata-local dial core", "error", err, "retry_in", backoff)
		timer := time.NewTimer(backoff)
		select {
		case <-ctx.Done():
			timer.Stop()
			return nil
		case <-timer.C:
		}
		if backoff *= 2; backoff > 5*time.Second {
			backoff = 5 * time.Second
		}
	}
}
