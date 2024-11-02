package main

import (
	"context"
	"fmt"
	"log"
	"log/slog"
	"net/http"
	"sync"
	"time"
)

//=====================================================================

// EventServer manages Server-Sent Events (SSE) by handling client connections.
// Each client is represented by a channel of byte slices in the 'clients' map.
// When a client connects a new channel and the connections is added to the map to monitor closures.
// Clients receive messages over their individual channels, and the server uses the HTTP connection
// (via http.ResponseWriter) to push data to them over the established SSE connection.
type EventServer struct {
	context       context.Context
	cancel        context.CancelFunc
	ConnectClient chan chan []byte
	CloseClient   chan chan []byte
	clients       map[chan []byte]struct{} // Map to keep track of connected clients
	sync          sync.Mutex
	subscriber    Subscriber
	logger        *slog.Logger

	srvWg sync.WaitGroup
	reqWg sync.WaitGroup

	//Go-routine tracking fields
	goroutineTracker map[int]string
	trackerMutex     sync.Mutex
	goroutineID      int
}

func NewSSEServer(parentCtx context.Context, sub Subscriber, logger *slog.Logger) *EventServer {
	// Create a cancellable context derived from the parent context
	ctx, cancel := context.WithCancel(parentCtx)

	return &EventServer{
		context:       ctx,
		cancel:        cancel, // Store the cancel function to stop the server later
		ConnectClient: make(chan chan []byte),
		CloseClient:   make(chan chan []byte),
		clients:       make(map[chan []byte]struct{}),
		subscriber:    sub,
		logger:        logger,

		goroutineTracker: make(map[int]string),
		goroutineID:      0,
	}
}

func (sseServer *EventServer) Run() {
	sseServer.logger.Info("Starting server")
	defer sseServer.srvWg.Done()

	for {
		select {
		case <-sseServer.context.Done():
			sseServer.logger.Info("server shutdown context called-- %s", "err", sseServer.context.Err())

			sseServer.reqWg.Wait() // Wait for the request routines to finish

			return

		case clientConnection := <-sseServer.ConnectClient:
			sseServer.sync.Lock()
			sseServer.clients[clientConnection] = struct{}{}
			sseServer.logger.Debug("connecting client", "client", clientConnection)
			log.Printf("connecting client %v", clientConnection)
			sseServer.sync.Unlock()

		case clientDisconnect := <-sseServer.CloseClient:
			sseServer.sync.Lock()
			sseServer.logger.Debug("closing client", "client", sseServer.clients[clientDisconnect])
			log.Printf("closing client %v", clientDisconnect)
			delete(sseServer.clients, clientDisconnect)
			sseServer.sync.Unlock()
		}
	}
}

func (sseServer *EventServer) Stop() {
	sseServer.logger.Info("Waiting for server routines to finish")
	for client := range sseServer.clients {
		sseServer.logger.Debug("closing client", "client", client)
		close(client)
		delete(sseServer.clients, client)
	}
	sseServer.srvWg.Wait()
	sseServer.cancel()
}

func (sseServer *EventServer) HandleConnection(w http.ResponseWriter, req *http.Request) {

	// Set headers to mimic SSE
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no")
	w.Header().Set("Access-Control-Allow-Origin", "*")
	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "Streaming unsupported", http.StatusInternalServerError)
		return
	}

	ctx, cancel := context.WithCancel(req.Context())
	defer cancel() // Ensure that cancel is called when done

	message := make(chan []byte)
	sseServer.ConnectClient <- message

	//keeping the connection alive with keep-alive protocol
	keepAliveTicker := time.NewTicker(15 * time.Second)
	keepAliveMsg := []byte(":keepalive\n\n")

	sseServer.reqWg.Add(1)
	sseServer.TrackGoRoutine("request handler routine", func() {
		defer sseServer.reqWg.Done()
		defer keepAliveTicker.Stop()
		select {
		case <-ctx.Done():
			sseServer.logger.Info("request context done")
			sseServer.CloseClient <- message
		case <-sseServer.context.Done():
			sseServer.logger.Info("server context done")
			return
		}
	})

	clientPathRequested := req.URL.Path
	sseServer.logger.Debug("client path requested", slog.String("path", clientPathRequested))

	sseServer.reqWg.Add(1)
	sseServer.TrackGoRoutine("subscribe routine", func() {
		sseServer.subscriber.Subscribe(ctx, "*", message, &sseServer.reqWg)
	})

	// Loop to handle sending messages or keep-alive signals
	for {
		select {
		case msg, ok := <-message:
			if !ok {
				return
			}
			_, err := fmt.Fprintf(w, "data: %s\n\n", msg)
			if err != nil {
				log.Printf("error writing messeage: %v", err)
				return
			}
			flusher.Flush()
		case <-keepAliveTicker.C:
			// Send the keep-alive ping
			_, err := w.Write(keepAliveMsg)
			if err != nil {
				sseServer.logger.Debug("error writing keepalive", "err", err)
				return
			}
			flusher.Flush()
		case <-ctx.Done():
			return
		case <-sseServer.context.Done():
			return
		}
	}

}

//===================================================================
// Tracking and debugging go-routines for Event server
//===================================================================

func (sseServer *EventServer) TrackGoRoutine(name string, f func()) {

	sseServer.trackerMutex.Lock()
	id := sseServer.goroutineID
	sseServer.goroutineID++
	sseServer.goroutineTracker[id] = name
	sseServer.logger.Debug("started goroutine--", slog.Int("ID", id), slog.String("name", name))
	sseServer.trackerMutex.Unlock()

	go func() {
		defer func() {
			sseServer.trackerMutex.Lock()
			delete(sseServer.goroutineTracker, id)
			sseServer.logger.Debug("finished goroutine--", slog.Int("ID", id), slog.String("name", name))
			sseServer.trackerMutex.Unlock()
		}()
		f()
	}()
}

func (sseServer *EventServer) LogActiveGoRoutines() {
	sseServer.trackerMutex.Lock()
	defer sseServer.trackerMutex.Unlock()
	sseServer.logger.Debug("go routines left in tracker", slog.Int("total", len(sseServer.goroutineTracker)))
	sseServer.logger.Debug("go routines in tracker: ")
	for id, name := range sseServer.goroutineTracker {
		sseServer.logger.Debug("tracking goroutine", slog.String("goroutine", name), slog.Int("ID", id))
	}
}

//===========================================
// Subscribing logic
//===========================================

type Subscriber interface {
	Subscribe(ctx context.Context, channel string, messageChan chan []byte, wg *sync.WaitGroup)
}

type FakeRedis struct {
	Input chan string
}

func (fs *FakeRedis) Subscribe(ctx context.Context, channel string, messageChan chan []byte, wg *sync.WaitGroup) {
	defer wg.Done()
	// Send 5 messages to simulate Redis messages
	for i := 0; i < 5; i++ {
		select {
		case <-ctx.Done():
			log.Println("Context done, stopping message generation")
			return
		default:
			// Create a test message and send it to the message channel
			message := fmt.Sprintf("Test message %d", i)
			messageChan <- []byte(message) // Send to SSE's message channel
			time.Sleep(1 * time.Second)    // Simulate delay between messages
		}
	}
}
