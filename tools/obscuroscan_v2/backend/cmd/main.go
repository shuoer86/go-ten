package main

import (
	"context"
	"fmt"
	"log"
	"os"
	"os/signal"
	"time"

	"github.com/ten-protocol/go-ten/tools/obscuroscan_v2/backend/container"
)

func main() {
	cliConfig := parseCLIArgs()
	obsScanContainer, err := container.NewObscuroScanContainer(cliConfig)
	if err != nil {
		panic(err)
	}

	err = obsScanContainer.Start()
	if err != nil {
		panic(err)
	}

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, os.Interrupt)

	ctx, cancel := context.WithCancel(context.Background())

	go func() {
		oscall := <-sigCh
		log.Printf("OS interrupt:%+v\n", oscall)
		cancel()
	}()

	<-ctx.Done()

	fmt.Println("Stopping server...")
	go func() {
		time.Sleep(5 * time.Second)
		fmt.Println("Failed to stop after 5 seconds. Exiting.")
		os.Exit(1)
	}()

	err = obsScanContainer.Stop()
	if err != nil {
		fmt.Printf("failed to stop gracefully - %s\n", err)
		os.Exit(1)
	}

	// Graceful shutdown complete
	os.Exit(0)
}
