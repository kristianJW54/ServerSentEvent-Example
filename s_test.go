package main

import (
	"context"
	"fmt"
	"io"
	"log"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"runtime"
	"testing"
	"time"
)

func TestSSEServer(t *testing.T) {
	testContext, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	mockRedis := &FakeRedis{Input: make(chan string)}

	// Initialize logger
	logHandler := slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{
		Level:     slog.LevelDebug,
		AddSource: false,
	})

	logger := slog.New(logHandler)

	sse := NewSSEServer(testContext, mockRedis, logger)

	sse.srvWg.Add(1)
	sse.TrackGoRoutine("main-server", func() {
		sse.Run()
	})

	testServer := httptest.NewUnstartedServer(http.HandlerFunc(sse.HandleConnection))
	testServer.Start()
	defer testServer.Close()

	// Perform the mock request
	err := MockRequest(t, &http.Client{}, testServer.URL)
	if err != nil {
		t.Fatal(err)
	}

	// Allow some time for the request to be processed
	time.Sleep(1 * time.Second) // Optional: adjust the sleep time as necessary

	// Test shutdown of SSE after the timeout
	<-testContext.Done()
	sse.Stop()

	sse.LogActiveGoRoutines()
	log.Println("Number of goroutines:", runtime.NumGoroutine())
}

func MockRequest(t *testing.T, client *http.Client, url string) error {
	t.Helper()

	// Make the GET request
	resp, err := client.Get(url + "/events/test")
	if err != nil {
		return fmt.Errorf("failed to make request: %w", err)
	}
	defer resp.Body.Close() // Ensure the body is closed after reading

	// Read messages from the response
	buf := make([]byte, 1024)
	//msg := make(chan []byte)

	for {
		n, err := resp.Body.Read(buf)
		if err != nil {
			if err == io.EOF {
				break // End of stream
			}
			return fmt.Errorf("error reading from response body: %w", err)
		}

		message := buf[:n]
		t.Logf("Received message - sending to client")
		t.Logf("%s", message)
	}

	return nil // Return nil if everything was successful
}
