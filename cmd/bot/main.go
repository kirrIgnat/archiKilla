package main

import (
	"artem-study/internal/app"
	"artem-study/internal/bot"
	"context"
	"errors"
	"log"
	"os/signal"
	"syscall"
	"time"
)

func main() {
	c, e := app.LoadConfig()
	if e != nil {
		log.Fatal(e)
	}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	startup, cancel := context.WithTimeout(ctx, 15*time.Second)
	a, e := app.Open(startup, c)
	cancel()
	if e != nil {
		log.Fatal(e)
	}
	defer a.DB.Close()
	if e = bot.New(a).Run(ctx); e != nil && !errors.Is(e, context.Canceled) {
		log.Fatal(e)
	}
}
