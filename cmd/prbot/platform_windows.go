//go:build windows

package main

import (
	"context"
	"os"
	"os/signal"

	"golang.org/x/sys/windows/svc"
)

const serviceName = "prbot"

func isService() bool {
	ok, err := svc.IsWindowsService()
	return err == nil && ok
}

// runPlatform runs under the Windows Service Control Manager when started as
// a service, otherwise in the console until Ctrl+C.
func runPlatform(run func(ctx context.Context) error) error {
	if isService() {
		return svc.Run(serviceName, &service{run: run})
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	return run(ctx)
}

type service struct {
	run func(ctx context.Context) error
}

func (s *service) Execute(_ []string, req <-chan svc.ChangeRequest, status chan<- svc.Status) (bool, uint32) {
	const accepts = svc.AcceptStop | svc.AcceptShutdown
	status <- svc.Status{State: svc.StartPending}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- s.run(ctx) }()
	status <- svc.Status{State: svc.Running, Accepts: accepts}
	for {
		select {
		case c := <-req:
			switch c.Cmd {
			case svc.Interrogate:
				status <- c.CurrentStatus
			case svc.Stop, svc.Shutdown:
				status <- svc.Status{State: svc.StopPending}
				cancel()
				<-done
				return false, 0
			}
		case err := <-done:
			if err != nil {
				return false, 1
			}
			return false, 0
		}
	}
}
