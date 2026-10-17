package main

import (
	"artem-study/internal/app"
	"artem-study/internal/web"
	"context"
	"errors"
	"log"
	"net/http"
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
	startup, cancel := context.WithTimeout(ctx, 30*time.Second)
	a, e := app.Open(startup, c)
	if e != nil {
		cancel()
		log.Fatal(e)
	}
	defer a.DB.Close()
	if e = a.Migrate(startup); e != nil {
		log.Fatalf("migration failed (%T)", e)
	}
	if e = a.Bootstrap(startup); e != nil {
		log.Fatal(e)
	}
	cancel()
	srv := &http.Server{Addr: c.Addr, Handler: a.Handler(web.Handler()), ReadHeaderTimeout: 5 * time.Second, ReadTimeout: 15 * time.Second, WriteTimeout: 30 * time.Second, IdleTimeout: 60 * time.Second}
	done := make(chan error, 1)
	go func() { log.Printf("Artem Study listening on %s", c.Addr); done <- srv.ListenAndServe() }()
	select {
	case e = <-done:
		if !errors.Is(e, http.ErrServerClosed) {
			log.Fatal(e)
		}
	case <-ctx.Done():
		shutdown, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if e = srv.Shutdown(shutdown); e != nil {
			log.Print(e)
		}
	}
}
