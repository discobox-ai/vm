// Command guestserver is the server internal/e2e's TestURL runs in a guest
// whose ingress URL is HTTP: it listens where the guest's service file says
// (guest.ListenService) and answers every request with a greeting.
package main

import (
	"fmt"
	"log"
	"net/http"
	"time"

	"github.com/discobox-ai/vm/pkg/guest"
)

// Greeting is what every request is answered.
const Greeting = "hello from the service"

func main() {
	l, err := guest.ListenService()
	if err != nil {
		log.Fatal(err)
	}
	server := &http.Server{
		Handler:           http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { fmt.Fprint(w, Greeting) }),
		ReadHeaderTimeout: 10 * time.Second,
	}
	log.Fatal(server.Serve(l))
}
