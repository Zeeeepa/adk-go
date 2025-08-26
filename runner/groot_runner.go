package runner

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"iter"
	"log"
	"os"
	"strings"

	"github.com/google/uuid"
	"google.golang.org/adk/agent"
	"google.golang.org/adk/internal/agent/parentmap"
	"google.golang.org/adk/internal/agent/runconfig"
	"google.golang.org/adk/internal/llminternal"
	"google.golang.org/adk/llm"
	"google.golang.org/adk/runner/internal"
	"google.golang.org/adk/session"
	"google.golang.org/adk/sessionservice"
	"google.golang.org/genai"
)

type GRootRunnerConfig struct {
	GRootEndpoint string
	GRootAPIKey   string

	EventLog       string
	ResumeEventLog bool

	AppName        string
	RootAgent      agent.Agent
	SessionService sessionservice.Service
}

type GRootRunner struct {
	cfg *GRootRunnerConfig

	hydratedEvents []*session.Event

	parents  parentmap.Map
	registry *internal.Registry
	eventLog *EventLog
}

func NewGRootRunner(cfg *GRootRunnerConfig) (*GRootRunner, error) {
	if cfg.SessionService == nil {
		cfg.SessionService = sessionservice.Mem()
	}
	if cfg.EventLog == "" {
		// TODO: Event log should be per app, and per user.
		// For this demo, we will use a single log file
		// assuming there is a single app and a single user.
		cfg.EventLog = "adk_runner.log"
	}
	client, err := internal.NewClient(cfg.GRootEndpoint, cfg.GRootAPIKey)
	if err != nil {
		return nil, err
	}
	var eventLog *EventLog
	var events []*session.Event
	if cfg.ResumeEventLog {
		eventLog, events, err = ResumerEventLog(cfg.EventLog, client)
		if err != nil {
			return nil, err
		}
	} else {
		eventLog, err = NewEventLog(cfg.AppName, cfg.EventLog, client)
		if err != nil {
			return nil, err
		}
	}
	return &GRootRunner{
		cfg:            cfg,
		hydratedEvents: events,
		eventLog:       eventLog,
		registry:       internal.NewRegistry(cfg.RootAgent),
	}, nil
}

func (r *GRootRunner) Run(ctx context.Context, userID, sessionID string, msg *genai.Content, cfg *RunConfig) iter.Seq2[*session.Event, error] {
	return func(yield func(*session.Event, error) bool) {
		// TODO: Replace with Get.
		session, err := r.cfg.SessionService.Create(ctx, &sessionservice.CreateRequest{
			AppName:   r.cfg.AppName,
			UserID:    userID,
			SessionID: sessionID,
		})
		if err != nil {
			yield(nil, err)
			return
		}

		agentToRun, err := r.findAgentToRun(session)
		if err != nil {
			yield(nil, err)
			return
		}

		if cfg != nil && cfg.SupportCFC {
			if err := r.setupCFC(agentToRun); err != nil {
				yield(nil, fmt.Errorf("failed to setup CFC: %w", err))
				return
			}
		}

		input := uuid.NewString()
		output := uuid.NewString()
		branch := input + ":" + output

		ctx = parentmap.ToContext(ctx, r.parents)
		ctx = runconfig.ToContext(ctx, &runconfig.RunConfig{
			StreamingMode: runconfig.StreamingMode(cfg.StreamingMode),
		})

		ctx := agent.NewContext(ctx, agentToRun, msg, &mutableSession{
			service:       r.cfg.SessionService,
			storedSession: session,
		}, branch)

		if err := r.appendMessageToSession(ctx, session, msg); err != nil {
			yield(nil, err)
			return
		}

		if err := r.eventLog.LogActivity(
			userID,
			"agent_start",
			r.registry.AgentFullname(agentToRun),
			input,
			output,
		); err != nil {
			log.Printf("Failed to log agent: %v", err)
		}
		for event, err := range agentToRun.Run(ctx) {
			if err != nil {
				if !yield(event, err) {
					return
				}
				continue
			}

			if err := r.eventLog.LogEvent(userID, output, event); err != nil {
				log.Printf("Failed to log event: %v", err)
			}
			// only commit non-partial event to a session service
			if !(event.LLMResponse != nil && event.LLMResponse.Partial) {
				// TODO: update session state & delta
				if err := r.cfg.SessionService.AppendEvent(ctx, session, event); err != nil {
					yield(nil, fmt.Errorf("failed to add event to session: %w", err))
					return
				}
			}

			if !yield(event, nil) {
				return
			}
		}
	}
}

// findAgentToRun returns the agent that should handle the next request based on
// session history.
func (r *GRootRunner) findAgentToRun(session sessionservice.StoredSession) (agent.Agent, error) {
	events := session.Events()
	for i := events.Len() - 1; i >= 0; i-- {
		event := events.At(i)

		// TODO: findMatchingFunctionCall.

		if event.Author == "user" {
			continue
		}

		subAgent := findAgent(r.cfg.RootAgent, event.Author)
		// Agent not found, continue looking for the other event.
		if subAgent == nil {
			log.Printf("Event from an unknown agent: %s, event id: %s", event.Author, event.ID)
			continue
		}

		if r.isTransferableAcrossAgentTree(subAgent) {
			return subAgent, nil
		}
	}

	// Falls back to root agent if no suitable agents are found in the session.
	return r.cfg.RootAgent, nil
}

// checks if the agent and its parent chain allow transfer up the tree.
func (r *GRootRunner) isTransferableAcrossAgentTree(agentToRun agent.Agent) bool {
	for curAgent := agentToRun; curAgent != nil; curAgent = r.parents[curAgent.Name()] {
		llmAgent, ok := agentToRun.(llminternal.Agent)
		if !ok {
			return false
		}
		if llminternal.Reveal(llmAgent).DisallowTransferToParent {
			return false
		}
	}

	return true
}

func (r *GRootRunner) setupCFC(curAgent agent.Agent) error {
	llmAgent, ok := curAgent.(llminternal.Agent)
	if !ok {
		return fmt.Errorf("agent %v is not an LLMAgent", curAgent.Name())
	}

	model := llminternal.Reveal(llmAgent).Model

	if model == nil {
		return fmt.Errorf("LLMAgent has no model")
	}

	if !strings.HasPrefix(model.Name(), "gemini-2") {
		return fmt.Errorf("CFC is not supported for model: %v", model.Name())
	}

	// TODO: handle CFC setup for LLMAgent, e.g. setting code_executor
	return nil
}

func (r *GRootRunner) appendMessageToSession(ctx agent.Context, storedSession sessionservice.StoredSession, msg *genai.Content) error {
	if msg == nil {
		return nil
	}
	event := session.NewEvent(ctx.InvocationID())

	event.Author = "user"
	event.LLMResponse = &llm.Response{
		Content: msg,
	}

	if err := r.cfg.SessionService.AppendEvent(ctx, storedSession, event); err != nil {
		return fmt.Errorf("failed to append event to sessionService: %w", err)
	}
	return nil
}

type EventLog struct {
	appName string
	logFile *os.File
	session *internal.Session
	shadows map[string]*internal.Shadow // by output ID
}

func NewEventLog(appName string, filename string, client *internal.Client) (*EventLog, error) {
	sess, err := client.OpenSession(uuid.NewString())
	if err != nil {
		return nil, err
	}
	file, err := os.OpenFile(filename, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0644)
	if err != nil {
		return nil, err
	}
	return &EventLog{
		appName: appName,
		logFile: file,
		session: sess,
		shadows: make(map[string]*internal.Shadow),
	}, nil
}

func (e *EventLog) LogEvent(userID string, id string, event *session.Event) error {
	if event.LLMResponse == nil || event.LLMResponse.Content == nil {
		return nil
	}
	shadow, ok := e.shadows[id]
	if !ok {
		return fmt.Errorf("no shadow found for output ID: %s", id)
	}

	out, err := json.Marshal(event)
	if err != nil {
		return err
	}
	if err := shadow.WriteFrame(id, &internal.Chunk{
		MIMEType: "application/json",
		Data:     out,
	}, event.Partial); err != nil {
		return err
	}
	fmt.Fprintf(e.logFile, "%s|%s|%s|stream|%s\n", e.appName, userID, e.session.ID(), id)
	e.logFile.Sync()
	return nil
}

func (e *EventLog) LogActivity(userID string, kind string, name, input, output string) error {
	shadow, err := e.session.NewADKShadow(name, input, output)
	if err != nil {
		return err
	}
	e.shadows[output] = shadow
	_, err = fmt.Fprintf(e.logFile, "%s|%s|%s|%s|%s|%s|%s\n", e.appName, userID, e.session.ID(), kind, name, input, output)
	e.logFile.Sync()
	return err
}

func ResumerEventLog(filename string, client *internal.Client) (*EventLog, []*session.Event, error) {
	file, err := os.OpenFile(filename, os.O_CREATE|os.O_APPEND|os.O_RDWR, 0644)
	if err != nil {
		return nil, nil, err
	}
	scanner := bufio.NewScanner(file)
	var sessionID string
	var eventIDs []string
	for scanner.Scan() {
		line := scanner.Text()
		parts := strings.Split(line, "|")
		switch parts[1] {
		case "agent_start":
		case "stream":
			sessionID = parts[0]
			eventIDs = append(eventIDs, parts[2])
		}
	}
	if err := scanner.Err(); err != nil {
		return nil, nil, err
	}
	sess, err := client.OpenSession(sessionID)
	if err != nil {
		return nil, nil, err
	}

	// Hydrate all events...
	var events []*session.Event
	for _, id := range eventIDs {
		chunks, err := sess.ReadAll(id)
		if err != nil {
			return nil, nil, err
		}
		for _, chunk := range chunks {
			if chunk == nil || chunk.Data == nil {
				continue
			}
			if chunk.MIMEType != "application/json" {
				return nil, nil, fmt.Errorf("expected application/json, got %s", chunk.MIMEType)
			}
			var event session.Event
			if err := json.Unmarshal(chunk.Data, &event); err != nil {
				return nil, nil, err
			}
			events = append(events, &event)
		}
	}
	return &EventLog{
		logFile: file,
		session: sess,
		shadows: make(map[string]*internal.Shadow),
	}, events, nil
}
