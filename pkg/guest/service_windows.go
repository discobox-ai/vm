package guest

import (
	"fmt"
	"os"
	"path/filepath"
	"time"

	"golang.org/x/sys/windows"
	"golang.org/x/sys/windows/svc"
)

// ServiceName is the agent's Windows service, as Install registers it.
const ServiceName = "disco-vm"

// RunAgent runs serve the way the guest OS starts its agent. Started by the
// service control manager, the agent reports to it, and a stop or a system
// shutdown calls stop, which must make serve return. Its output goes to
// guest.log beside the binary, since a service's stderr goes nowhere.
// Started any other way, it is serve.
func RunAgent(serve func() error, stop func()) error {
	isService, err := svc.IsWindowsService()
	if err != nil || !isService {
		return serve()
	}
	if exe, err := os.Executable(); err == nil {
		if f, err := os.OpenFile(filepath.Join(filepath.Dir(exe), "guest.log"), os.O_WRONLY|os.O_CREATE|os.O_APPEND, 0o600); err == nil {
			os.Stdout, os.Stderr = f, f
			_ = windows.SetStdHandle(windows.STD_ERROR_HANDLE, windows.Handle(f.Fd()))
		}
	}
	fmt.Fprintf(os.Stderr, "%s agent: starting as service %s\n", time.Now().Format(time.RFC3339), ServiceName)
	return svc.Run(ServiceName, &agentService{serve: serve, stop: stop})
}

type agentService struct {
	serve func() error
	stop  func()
}

func (a *agentService) Execute(_ []string, requests <-chan svc.ChangeRequest, status chan<- svc.Status) (bool, uint32) {
	status <- svc.Status{State: svc.StartPending}
	served := make(chan error, 1)
	go func() { served <- a.serve() }()
	status <- svc.Status{State: svc.Running, Accepts: svc.AcceptStop | svc.AcceptShutdown}
	for {
		select {
		case err := <-served:
			// The agent stopped on its own: say so, and let the recovery
			// actions restart it.
			fmt.Fprintf(os.Stderr, "%s agent: stopped: %v\n", time.Now().Format(time.RFC3339), err)
			return true, 1
		case req := <-requests:
			switch req.Cmd {
			case svc.Interrogate:
				status <- req.CurrentStatus
			case svc.Stop, svc.Shutdown:
				status <- svc.Status{State: svc.StopPending}
				a.stop()
				select {
				case <-served:
				case <-time.After(10 * time.Second):
				}
				return false, 0
			}
		}
	}
}
