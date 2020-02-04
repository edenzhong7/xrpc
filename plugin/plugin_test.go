package plugin_test

import (
	"context"
	"math/rand"
	"testing"
	"time"

	"github.com/edenzhong7/xrpc"
	"github.com/edenzhong7/xrpc/plugin"
	"github.com/edenzhong7/xrpc/plugin/log"
	"github.com/edenzhong7/xrpc/plugin/prom"
)

func TestLogPlugin(t *testing.T) {
	var (
		logPlugin = log.New()
		pc        = plugin.NewPluginContainer()
	)
	pc.Add(logPlugin)
	pc.DoPreWriteResponse(nil, nil, nil)
	pc.Remove(logPlugin)
	println()
}

func TestPromPlugin(t *testing.T) {
	var (
		promPlugin = prom.New()
		pc         = plugin.NewPluginContainer()
	)
	pc.Add(promPlugin)
	ctx := context.Background()
	info := &xrpc.UnaryServerInfo{
		Server:     nil,
		FullMethod: "/greeter.Greeter/SayHello",
	}
	handler := func(ctx context.Context, req interface{}) (interface{}, error) {
		time.Sleep(time.Duration(rand.Intn(12)) * time.Millisecond)
		return nil, nil
	}
	for i := 0; i < 100; i++ {
		pc.DoHandle(ctx, nil, info, handler)
	}
}