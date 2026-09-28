//go:build ignore

// Smoke: router against a real llama.cpp server. go run smoke.go <baseURL> <model>
package main

import (
	"context"
	"fmt"
	"os"
	"time"

	"github.com/lararium-app/lararium/internal/router"
)

func main() {
	base, model := os.Args[1], os.Args[2]
	p := router.NewOpenAI(base, "", model)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	caps, err := p.Capabilities(ctx)
	fmt.Printf("caps: ctx=%d vision=%v tools=%v source=%s err=%v\n",
		caps.ContextLength, caps.SupportsVision, caps.SupportsTools, caps.Source, err)

	c, err := p.Complete(ctx, []router.Message{{Role: router.RoleUser, Content: "Reply with exactly: PENATUS-OK"}}, router.Options{MaxTokens: 2000})
	if err != nil {
		fmt.Println("complete error:", err)
		os.Exit(1)
	}
	fmt.Printf("text=%q in=%d out=%d probed=%v model=%s\n", c.Text, c.InTokens, c.OutTokens, c.Probed, c.Model)
}
