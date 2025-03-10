package server

import (
	"context"
	"sync"

	"github.com/wandb/wandb/nexus/internal/shared"
	"github.com/wandb/wandb/nexus/pkg/observability"
	"github.com/wandb/wandb/nexus/pkg/service"
)

const (
	internalConnectionId = "internal"
)

// Stream is a collection of components that work together to handle incoming
// data for a W&B run, store it locally, and send it to a W&B server.
// Stream.handler receives incoming data from the client and dispatches it to
// Stream.writer, which writes it to a local file. Stream.writer then sends the
// data to Stream.sender, which sends it to the W&B server. Stream.dispatcher
// handles dispatching responses to the appropriate client responders.
type Stream struct {
	// ctx is the context for the stream
	ctx context.Context

	// wg is the WaitGroup for the stream
	wg sync.WaitGroup

	// handler is the handler for the stream
	handler *Handler

	// dispatcher is the dispatcher for the stream
	dispatcher *Dispatcher

	// writer is the writer for the stream
	writer *Writer

	// sender is the sender for the stream
	sender *Sender

	// settings is the settings for the stream
	settings *service.Settings

	// logger is the logger for the stream
	logger *observability.NexusLogger

	// inChan is the channel for incoming messages
	inChan chan *service.Record

	// loopbackChan is the channel for internal loopback messages
	loopbackChan chan *service.Record

	// internal responses from teardown path typically
	respChan chan *service.ServerResponse
}

// NewStream creates a new stream with the given settings and responders.
func NewStream(ctx context.Context, settings *service.Settings, streamId string) *Stream {
	logFile := settings.GetLogInternal().GetValue()
	logger := SetupStreamLogger(logFile, settings)

	stream := &Stream{
		ctx:      ctx,
		wg:       sync.WaitGroup{},
		settings: settings,
		logger:   logger,
		inChan:   make(chan *service.Record, BufferSize),
		respChan: make(chan *service.ServerResponse, BufferSize),
	}
	stream.Start()
	return stream
}

// AddResponders adds the given responders to the stream's dispatcher.
func (s *Stream) AddResponders(entries ...ResponderEntry) {
	s.dispatcher.AddResponders(entries...)
}

// Start starts the stream's handler, writer, sender, and dispatcher.
// We use Stream's wait group to ensure that all of these components are cleanly
// finalized and closed when the stream is closed in Stream.Close().
func (s *Stream) Start() {
	s.logger.Info("created new stream", "id", s.settings.RunId)

	s.loopbackChan = make(chan *service.Record, BufferSize)
	s.handler = NewHandler(s.ctx, s.settings, s.logger, s.loopbackChan)
	s.writer = NewWriter(s.ctx, s.settings, s.logger)
	s.sender = NewSender(s.ctx, s.settings, s.logger, s.loopbackChan)
	s.dispatcher = NewDispatcher(s.logger)

	// handle the client requests
	s.wg.Add(1)
	go func() {
		s.handler.do(s.inChan, s.sender.loopbackChan)
		s.wg.Done()
	}()

	// write the data to a transaction log
	s.wg.Add(1)
	go func() {
		s.writer.do(s.handler.fwdChan)
		s.wg.Done()
	}()

	// send the data to the server
	s.wg.Add(1)
	go func() {
		s.sender.do(s.writer.fwdChan)
		s.wg.Done()
	}()

	// handle dispatching between components
	s.wg.Add(1)
	go func() {
		s.dispatcher.do(s.handler.outChan, s.sender.outChan)
		s.wg.Done()
	}()

	s.logger.Debug("starting stream", "id", s.settings.RunId)
}

// HandleRecord handles the given record by sending it to the stream's handler.
func (s *Stream) HandleRecord(rec *service.Record) {
	s.logger.Debug("handling record", "record", rec)
	s.inChan <- rec
}

func (s *Stream) GetRun() *service.RunRecord {
	return s.handler.GetRun()
}

// Close Gracefully wait for handler, writer, sender, dispatcher to shut down cleanly
// assumes an exit record has already been sent
func (s *Stream) Close() {
	s.handler.Close()
	s.writer.Close()
	s.sender.Close()
	s.wg.Wait()
}

// Respond Handle internal responses like from the finish and close path
func (s *Stream) Respond(resp *service.ServerResponse) {
	s.respChan <- resp
}

func (s *Stream) FinishAndClose(exitCode int32) {
	s.AddResponders(ResponderEntry{s, internalConnectionId})

	// send exit record to handler
	record := &service.Record{
		RecordType: &service.Record_Exit{
			Exit: &service.RunExitRecord{
				ExitCode: exitCode,
			}},
		Control: &service.Control{AlwaysSend: true, ConnectionId: internalConnectionId, ReqResp: true},
	}

	s.HandleRecord(record)
	// TODO(beta): process the response so we can formulate a more correct footer
	<-s.respChan

	// send a shutdown which triggers the handler to stop processing new records
	shutdownRecord := &service.Record{
		RecordType: &service.Record_Request{
			Request: &service.Request{
				RequestType: &service.Request_Shutdown{
					Shutdown: &service.ShutdownRequest{},
				},
			}},
		Control: &service.Control{AlwaysSend: true, ConnectionId: internalConnectionId, ReqResp: true},
	}
	s.HandleRecord(shutdownRecord)
	<-s.respChan

	s.Close()

	s.PrintFooter()
	s.logger.Info("closed stream", "id", s.settings.RunId)
}

func (s *Stream) PrintFooter() {
	run := s.GetRun()
	shared.PrintHeadFoot(run, s.settings, true)
}
