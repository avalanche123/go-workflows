package workflow

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"time"

	"github.com/benbjohnson/clock"
	"github.com/cschleiden/go-workflows/internal/command"
	"github.com/cschleiden/go-workflows/internal/core"
	"github.com/cschleiden/go-workflows/internal/history"
	"github.com/cschleiden/go-workflows/internal/payload"
	"github.com/cschleiden/go-workflows/internal/sync"
	"github.com/cschleiden/go-workflows/internal/task"
	"github.com/cschleiden/go-workflows/internal/workflowstate"
	"github.com/cschleiden/go-workflows/log"
)

type ExecutionResult struct {
	Completed      bool
	Executed       []history.Event
	ActivityEvents []history.Event
	WorkflowEvents []history.WorkflowEvent
}

type WorkflowHistoryProvider interface {
	GetWorkflowInstanceHistory(ctx context.Context, instance *core.WorkflowInstance, lastSequenceID *int64) ([]history.Event, error)
}

type WorkflowExecutor interface {
	ExecuteTask(ctx context.Context, t *task.Workflow) (*ExecutionResult, error)

	Close()
}

type executor struct {
	registry          *Registry
	historyProvider   WorkflowHistoryProvider
	workflow          *workflow
	workflowState     *workflowstate.WfState
	workflowCtx       sync.Context
	workflowCtxCancel sync.CancelFunc
	clock             clock.Clock
	logger            log.Logger
	lastSequenceID    int64
}

func NewExecutor(logger log.Logger, registry *Registry, historyProvider WorkflowHistoryProvider, instance *core.WorkflowInstance, clock clock.Clock) (WorkflowExecutor, error) {
	s := workflowstate.NewWorkflowState(instance, logger, clock)
	wfCtx, cancel := sync.WithCancel(workflowstate.WithWorkflowState(sync.Background(), s))

	return &executor{
		registry:          registry,
		historyProvider:   historyProvider,
		workflowState:     s,
		workflowCtx:       wfCtx,
		workflowCtxCancel: cancel,
		clock:             clock,
		logger:            logger,
	}, nil
}

func (e *executor) ExecuteTask(ctx context.Context, t *task.Workflow) (*ExecutionResult, error) {
	e.logger.Debug("Executing workflow task", "task_id", t.ID, "instance_id", t.WorkflowInstance.InstanceID)

	e.workflowState.ClearCommands()

	skipNewEvents := false

	if t.LastSequenceID > e.lastSequenceID {
		e.logger.Debug("Task has newer history than current state, fetching and replaying history", "task_sequence_id", t.LastSequenceID, "sequence_id", e.lastSequenceID)

		h, err := e.historyProvider.GetWorkflowInstanceHistory(ctx, t.WorkflowInstance, &e.lastSequenceID)
		if err != nil {
			return nil, fmt.Errorf("getting workflow history: %w", err)
		}

		if err := e.replayHistory(h); err != nil {
			e.logger.Error("Error while replaying history", "error", err)

			// Fail workflow with an error. Skip executing new events, but still go through the commands
			e.workflowCompleted(nil, err)
			skipNewEvents = true
		}

		if t.LastSequenceID != e.lastSequenceID {
			return nil, errors.New("even after fetching history and replaying history executor state does not match task")
		}
	} else if t.LastSequenceID < e.lastSequenceID {
		return nil, fmt.Errorf("task has older history than current state, cannot execute")
	}

	// Always add a WorkflowTaskStarted event before executing new tasks
	toExecute := []history.Event{e.createNewEvent(history.EventType_WorkflowTaskStarted, &history.WorkflowTaskStartedAttributes{})}
	executedEvents := toExecute

	toExecute = append(toExecute, t.NewEvents...)

	// Execute new events received from the backend
	if !skipNewEvents {
		var err error
		executedEvents, err = e.executeNewEvents(toExecute)
		if err != nil {
			e.logger.Error("Error while executing new events", "error", err)

			e.workflowCompleted(nil, err)
		}
	}

	// Process any commands added while executing new events
	completed, newCommandEvents, activityEvents, workflowEvents, err := e.processCommands(ctx, t)
	if err != nil {
		return nil, fmt.Errorf("processing commands: %w", err)
	}

	executedEvents = append(executedEvents, newCommandEvents...)

	// Set SequenceIDs for all executed events
	for i := range executedEvents {
		executedEvents[i].SequenceID = e.nextSequenceID()
	}

	e.logger.Debug("Finished workflow task",
		"task_id", t.ID,
		"instance_id", t.WorkflowInstance.InstanceID,
		"executed", len(executedEvents),
		"last_sequence_id", e.lastSequenceID,
		"completed", completed,
	)

	return &ExecutionResult{
		Completed:      completed,
		Executed:       executedEvents,
		ActivityEvents: activityEvents,
		WorkflowEvents: workflowEvents,
	}, nil
}

func (e *executor) replayHistory(history []history.Event) error {
	e.workflowState.SetReplaying(true)
	for _, event := range history {
		if err := e.executeEvent(event); err != nil {
			return err
		}

		e.lastSequenceID = event.SequenceID
	}

	return nil
}

func (e *executor) executeNewEvents(newEvents []history.Event) ([]history.Event, error) {
	e.workflowState.SetReplaying(false)

	for i, event := range newEvents {
		if err := e.executeEvent(event); err != nil {
			return newEvents[:i], err
		}
	}

	if e.workflow.Completed() {
		e.workflowCompleted(e.workflow.Result(), e.workflow.Error())
	}

	return newEvents, nil
}

func (e *executor) Close() {
	if e.workflow != nil {
		// End workflow if running to prevent leaking goroutines
		e.workflow.Close(e.workflowCtx)
	}
}

func (e *executor) executeEvent(event history.Event) error {
	e.logger.Debug("Executing event",
		"instance_id", e.workflowState.Instance().InstanceID,
		"event_id", event.ID,
		"seq_id", event.SequenceID,
		"event_type", event.Type,
	)

	var err error

	switch event.Type {
	case history.EventType_WorkflowExecutionStarted:
		err = e.handleWorkflowExecutionStarted(event.Attributes.(*history.ExecutionStartedAttributes))

	case history.EventType_WorkflowExecutionFinished:
	// Ignore

	case history.EventType_WorkflowExecutionCanceled:
		err = e.handleWorkflowCanceled()

	case history.EventType_WorkflowTaskStarted:
		err = e.handleWorkflowTaskStarted(event, event.Attributes.(*history.WorkflowTaskStartedAttributes))

	case history.EventType_ActivityScheduled:
		err = e.handleActivityScheduled(event, event.Attributes.(*history.ActivityScheduledAttributes))

	case history.EventType_ActivityFailed:
		err = e.handleActivityFailed(event, event.Attributes.(*history.ActivityFailedAttributes))

	case history.EventType_ActivityCompleted:
		err = e.handleActivityCompleted(event, event.Attributes.(*history.ActivityCompletedAttributes))

	case history.EventType_TimerScheduled:
		err = e.handleTimerScheduled(event, event.Attributes.(*history.TimerScheduledAttributes))

	case history.EventType_TimerFired:
		err = e.handleTimerFired(event, event.Attributes.(*history.TimerFiredAttributes))

	case history.EventType_TimerCanceled:
		err = e.handleTimerCanceled(event, event.Attributes.(*history.TimerCanceledAttributes))

	case history.EventType_SignalReceived:
		err = e.handleSignalReceived(event, event.Attributes.(*history.SignalReceivedAttributes))

	case history.EventType_SideEffectResult:
		err = e.handleSideEffectResult(event, event.Attributes.(*history.SideEffectResultAttributes))

	case history.EventType_SubWorkflowScheduled:
		err = e.handleSubWorkflowScheduled(event, event.Attributes.(*history.SubWorkflowScheduledAttributes))
	case history.EventType_SubWorkflowCancellationRequested:
		err = e.handleSubWorkflowCancellationRequest(event, event.Attributes.(*history.SubWorkflowCancellationRequestedAttributes))
	case history.EventType_SubWorkflowFailed:
		err = e.handleSubWorkflowFailed(event, event.Attributes.(*history.SubWorkflowFailedAttributes))
	case history.EventType_SubWorkflowCompleted:
		err = e.handleSubWorkflowCompleted(event, event.Attributes.(*history.SubWorkflowCompletedAttributes))

	default:
		return fmt.Errorf("unknown event type: %v", event.Type)
	}

	return err
}

func (e *executor) handleWorkflowExecutionStarted(a *history.ExecutionStartedAttributes) error {
	wfFn, err := e.registry.GetWorkflow(a.Name)
	if err != nil {
		return fmt.Errorf("workflow %s not found", a.Name)
	}

	e.workflow = NewWorkflow(reflect.ValueOf(wfFn))

	return e.workflow.Execute(e.workflowCtx, a.Inputs)
}

func (e *executor) handleWorkflowCanceled() error {
	e.workflowCtxCancel()

	return e.workflow.Continue(e.workflowCtx)
}

func (e *executor) handleWorkflowTaskStarted(event history.Event, a *history.WorkflowTaskStartedAttributes) error {
	e.workflowState.SetTime(event.Timestamp)

	return nil
}

func (e *executor) handleActivityScheduled(event history.Event, a *history.ActivityScheduledAttributes) error {
	c := e.workflowState.RemoveCommandByEventID(event.ScheduleEventID)

	// Ensure activity
	if c == nil {
		return fmt.Errorf("previous workflow execution scheduled an activity which could not be found")
	}

	if c.Type != command.CommandType_ScheduleActivity {
		return fmt.Errorf("previous workflow execution scheduled an activity, this time: %v", c.Type)
	}

	// Ensure the same activity was scheduled again
	ca := c.Attr.(*command.ScheduleActivityTaskCommandAttr)
	if a.Name != ca.Name {
		return fmt.Errorf("previous workflow execution scheduled different type of activity: %s, %s", a.Name, ca.Name)
	}

	return nil
}

func (e *executor) handleActivityCompleted(event history.Event, a *history.ActivityCompletedAttributes) error {
	f, ok := e.workflowState.FutureByScheduleEventID(event.ScheduleEventID)
	if !ok {
		return fmt.Errorf("could not find pending future for activity completion")
	}

	e.workflowState.RemoveCommandByEventID(event.ScheduleEventID)
	err := f(a.Result, nil)
	if err != nil {
		return fmt.Errorf("setting result: %w", err)
	}

	return e.workflow.Continue(e.workflowCtx)
}

func (e *executor) handleActivityFailed(event history.Event, a *history.ActivityFailedAttributes) error {
	f, ok := e.workflowState.FutureByScheduleEventID(event.ScheduleEventID)
	if !ok {
		return errors.New("no pending future for activity failed event")
	}

	e.workflowState.RemoveCommandByEventID(event.ScheduleEventID)

	if err := f(nil, errors.New(a.Reason)); err != nil {
		return fmt.Errorf("setting result: %w", err)
	}

	return e.workflow.Continue(e.workflowCtx)
}

func (e *executor) handleTimerScheduled(event history.Event, a *history.TimerScheduledAttributes) error {
	e.workflowState.RemoveCommandByEventID(event.ScheduleEventID)

	return nil
}

func (e *executor) handleTimerFired(event history.Event, a *history.TimerFiredAttributes) error {
	f, ok := e.workflowState.FutureByScheduleEventID(event.ScheduleEventID)
	if !ok {
		// Timer already canceled ignore
		return nil
	}

	c := e.workflowState.RemoveCommandByEventID(event.ScheduleEventID)
	if c == nil {
		return fmt.Errorf("previous workflow execution scheduled a timer")
	}

	if c.Type != command.CommandType_ScheduleTimer {
		return fmt.Errorf("previous workflow execution scheduled a timer, this time: %v", c.Type)
	}

	if err := f(nil, nil); err != nil {
		return fmt.Errorf("setting result: %w", err)
	}

	return e.workflow.Continue(e.workflowCtx)
}

func (e *executor) handleTimerCanceled(event history.Event, a *history.TimerCanceledAttributes) error {
	f, ok := e.workflowState.FutureByScheduleEventID(event.ScheduleEventID)
	if !ok {
		// Timer already canceled ignore
		return nil
	}

	e.workflowState.RemoveCommandByEventID(event.ScheduleEventID)

	if err := f(nil, nil); err != nil {
		return fmt.Errorf("setting result: %w", err)
	}

	return e.workflow.Continue(e.workflowCtx)
}

func (e *executor) handleSubWorkflowScheduled(event history.Event, a *history.SubWorkflowScheduledAttributes) error {
	c := e.workflowState.RemoveCommandByEventID(event.ScheduleEventID)
	if c == nil {
		return fmt.Errorf("previous workflow execution scheduled a sub workflow")
	}

	if c.Type != command.CommandType_ScheduleSubWorkflow {
		return fmt.Errorf("previous workflow execution scheduled a sub workflow, this time: %v", c.Type)
	}

	ca := c.Attr.(*command.ScheduleSubWorkflowCommandAttr)
	if a.Name != ca.Name {
		return fmt.Errorf("previous workflow execution scheduled different type of sub workflow: %s, %s", a.Name, ca.Name)
	}

	// Set correct InstanceID here.
	// TODO: see if we can provide better support for commands here and find a better place to store
	// this message.
	ca.Instance = a.SubWorkflowInstance

	return nil
}

func (e *executor) handleSubWorkflowCancellationRequest(event history.Event, a *history.SubWorkflowCancellationRequestedAttributes) error {
	c := e.workflowState.RemoveCommandByEventID(event.ScheduleEventID)
	if c == nil {
		return fmt.Errorf("previous workflow execution cancelled a sub-workflow execution")
	}

	if c.Type != command.CommandType_CancelSubWorkflow {
		return fmt.Errorf("previous workflow execution cancelled a sub-workflow execution, this time: %v", c.Type)
	}

	return e.workflow.Continue(e.workflowCtx)
}

func (e *executor) handleSubWorkflowFailed(event history.Event, a *history.SubWorkflowFailedAttributes) error {
	f, ok := e.workflowState.FutureByScheduleEventID(event.ScheduleEventID)
	if !ok {
		return errors.New("no pending future found for sub workflow failed event")
	}

	e.workflowState.RemoveCommandByEventID(event.ScheduleEventID)

	if err := f(nil, errors.New(a.Error)); err != nil {
		return fmt.Errorf("setting result: %w", err)
	}

	return e.workflow.Continue(e.workflowCtx)
}

func (e *executor) handleSubWorkflowCompleted(event history.Event, a *history.SubWorkflowCompletedAttributes) error {
	f, ok := e.workflowState.FutureByScheduleEventID(event.ScheduleEventID)
	if !ok {
		return errors.New("no pending future found for sub workflow completed event")
	}

	e.workflowState.RemoveCommandByEventID(event.ScheduleEventID)

	if err := f(a.Result, nil); err != nil {
		return fmt.Errorf("setting result: %w", err)
	}

	return e.workflow.Continue(e.workflowCtx)
}

func (e *executor) handleSignalReceived(event history.Event, a *history.SignalReceivedAttributes) error {
	// Send signal to workflow channel
	workflowstate.ReceiveSignal(e.workflowCtx, e.workflowState, a.Name, a.Arg)

	e.workflowState.RemoveCommandByEventID(event.ScheduleEventID)

	return e.workflow.Continue(e.workflowCtx)
}

func (e *executor) handleSideEffectResult(event history.Event, a *history.SideEffectResultAttributes) error {
	f, ok := e.workflowState.FutureByScheduleEventID(event.ScheduleEventID)
	if !ok {
		return errors.New("no pending future found for side effect result event")
	}

	f(a.Result, nil)

	return e.workflow.Continue(e.workflowCtx)
}

func (e *executor) workflowCompleted(result payload.Payload, err error) {
	eventId := e.workflowState.GetNextScheduleEventID()

	cmd := command.NewCompleteWorkflowCommand(eventId, result, err)
	e.workflowState.AddCommand(&cmd)
}

func (e *executor) processCommands(ctx context.Context, t *task.Workflow) (bool, []history.Event, []history.Event, []history.WorkflowEvent, error) {
	instance := t.WorkflowInstance
	commands := e.workflowState.Commands()

	completed := false
	newEvents := make([]history.Event, 0)
	activityEvents := make([]history.Event, 0)
	workflowEvents := make([]history.WorkflowEvent, 0)

	for _, c := range commands {
		// TODO: Move to state machine?
		// Mark this command as committed.
		c.State = command.CommandState_Committed

		switch c.Type {
		case command.CommandType_ScheduleActivity:
			a := c.Attr.(*command.ScheduleActivityTaskCommandAttr)

			scheduleActivityEvent := e.createNewEvent(
				history.EventType_ActivityScheduled,
				&history.ActivityScheduledAttributes{
					Name:   a.Name,
					Inputs: a.Inputs,
				},
				history.ScheduleEventID(c.ID),
			)

			newEvents = append(newEvents, scheduleActivityEvent)
			activityEvents = append(activityEvents, scheduleActivityEvent)

		case command.CommandType_ScheduleSubWorkflow:
			a := c.Attr.(*command.ScheduleSubWorkflowCommandAttr)

			newEvents = append(newEvents, e.createNewEvent(
				history.EventType_SubWorkflowScheduled,
				&history.SubWorkflowScheduledAttributes{
					SubWorkflowInstance: a.Instance,
					Name:                a.Name,
					Inputs:              a.Inputs,
				},
				history.ScheduleEventID(c.ID),
			))

			// Send message to new workflow instance
			workflowEvents = append(workflowEvents, history.WorkflowEvent{
				WorkflowInstance: a.Instance,
				HistoryEvent: e.createNewEvent(
					history.EventType_WorkflowExecutionStarted,
					&history.ExecutionStartedAttributes{
						Name:   a.Name,
						Inputs: a.Inputs,
					},
					history.ScheduleEventID(c.ID),
				),
			})

		case command.CommandType_CancelSubWorkflow:
			a := c.Attr.(*command.CancelSubWorkflowCommandAttr)

			// Record sub-workflow cancellation request event
			newEvents = append(newEvents, e.createNewEvent(
				history.EventType_SubWorkflowCancellationRequested,
				&history.SubWorkflowScheduledAttributes{
					SubWorkflowInstance: a.SubWorkflowInstance,
				},
			))

			// Send cancellation event to sub-workflow
			workflowEvents = append(workflowEvents, history.WorkflowEvent{
				WorkflowInstance: a.SubWorkflowInstance,
				HistoryEvent:     history.NewWorkflowCancellationEvent(time.Now()),
			})

		case command.CommandType_SideEffect:
			a := c.Attr.(*command.SideEffectCommandAttr)
			newEvents = append(newEvents, e.createNewEvent(
				history.EventType_SideEffectResult,
				&history.SideEffectResultAttributes{
					Result: a.Result,
				},
				history.ScheduleEventID(c.ID),
			))

		case command.CommandType_ScheduleTimer:
			a := c.Attr.(*command.ScheduleTimerCommandAttr)

			newEvents = append(newEvents, e.createNewEvent(
				history.EventType_TimerScheduled,
				&history.TimerScheduledAttributes{
					At: a.At,
				},
				history.ScheduleEventID(c.ID),
			))

			// Create timer_fired event which will become visible in the future
			workflowEvents = append(workflowEvents, history.WorkflowEvent{
				WorkflowInstance: instance,
				HistoryEvent: e.createNewEvent(
					history.EventType_TimerFired,
					&history.TimerFiredAttributes{
						At: a.At,
					},
					history.ScheduleEventID(c.ID),
					history.VisibleAt(a.At),
				)},
			)

		case command.CommandType_CancelTimer:
			a := c.Attr.(*command.CancelTimerCommandAttr)

			workflowEvents = append(workflowEvents, history.WorkflowEvent{
				WorkflowInstance: instance,
				HistoryEvent: e.createNewEvent(
					history.EventType_TimerCanceled,
					&history.TimerCanceledAttributes{},
					history.ScheduleEventID(a.TimerScheduleEventID),
				),
			})

		case command.CommandType_CompleteWorkflow:
			completed = true

			a := c.Attr.(*command.CompleteWorkflowCommandAttr)

			newEvents = append(newEvents, e.createNewEvent(
				history.EventType_WorkflowExecutionFinished,
				&history.ExecutionCompletedAttributes{
					Result: a.Result,
					Error:  a.Error,
				},
				history.ScheduleEventID(c.ID),
			))

			if instance.SubWorkflow() {
				// Send completion message back to parent workflow instance
				var historyEvent history.Event

				if a.Error != "" {
					// Sub workflow failed
					historyEvent = e.createNewEvent(
						history.EventType_SubWorkflowFailed,
						&history.SubWorkflowFailedAttributes{
							Error: a.Error,
						},
						// Ensure the message gets sent back to the parent workflow with the right eventID
						history.ScheduleEventID(instance.ParentEventID),
					)
				} else {
					historyEvent = e.createNewEvent(
						history.EventType_SubWorkflowCompleted,
						&history.SubWorkflowCompletedAttributes{
							Result: a.Result,
						},
						// Ensure the message gets sent back to the parent workflow with the right eventID
						history.ScheduleEventID(instance.ParentEventID),
					)
				}

				workflowEvents = append(workflowEvents, history.WorkflowEvent{
					WorkflowInstance: core.NewWorkflowInstance(instance.ParentInstanceID, ""), // TODO: Do we need execution id here?
					HistoryEvent:     historyEvent,
				})
			}

		default:
			return false, nil, nil, nil, fmt.Errorf("unknown command type: %v", c.Type)
		}
	}

	return completed, newEvents, activityEvents, workflowEvents, nil
}

func (e *executor) nextSequenceID() int64 {
	e.lastSequenceID++
	return e.lastSequenceID
}

func (e *executor) createNewEvent(eventType history.EventType, attributes interface{}, opts ...history.HistoryEventOption) history.Event {
	return history.NewPendingEvent(
		e.clock.Now(),
		eventType,
		attributes,
		opts...,
	)
}
