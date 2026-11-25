package main

import (
	"bambastic/internal/spoolbuddy"
	"context"
	"flag"
	"fmt"
	"log"
	"os"
	"os/signal"
	"syscall"
)

func main() {
	flag.Usage = func() {
		fmt.Fprintln(os.Stderr, "Usage: spoolbuddy\nSet SPOOLBUDDY_BACKEND_URL and, when authentication is enabled, SPOOLBUDDY_API_KEY. Override this station's identity using SPOOLBUDDY_DEVICE_ID and SPOOLBUDDY_HOSTNAME.")
	}
	flag.Parse()
	c, e := spoolbuddy.Environment()
	if e != nil {
		log.Fatal(e)
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if e = spoolbuddy.New(c, spoolbuddy.PiHardware()).Run(ctx); e != nil {
		log.Fatal(e)
	}
}
