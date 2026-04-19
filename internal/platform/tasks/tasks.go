package tasks

import (
	"fmt"
	"sync"
	"time"
)

type Event map[string]any

type Task struct {
	ID        string
	Total     int
	Processed int
	OK        int
	Fail      int
	Status    string
	Warning   string
	Result    map[string]any
	Error     string
	Cancelled bool
	CreatedAt time.Time

	mu     sync.Mutex
	queues []chan Event
	final  Event
}

func New(total int) *Task {
	return &Task{
		ID:        fmt.Sprintf("%d", time.Now().UnixNano()),
		Total:     total,
		Status:    "running",
		CreatedAt: time.Now(),
	}
}

func (t *Task) Attach() chan Event {
	t.mu.Lock()
	defer t.mu.Unlock()
	ch := make(chan Event, 200)
	t.queues = append(t.queues, ch)
	return ch
}

func (t *Task) Detach(target chan Event) {
	t.mu.Lock()
	defer t.mu.Unlock()
	next := make([]chan Event, 0, len(t.queues))
	for _, ch := range t.queues {
		if ch != target {
			next = append(next, ch)
		}
	}
	t.queues = next
}

func (t *Task) Snapshot() Event {
	t.mu.Lock()
	defer t.mu.Unlock()
	return Event{
		"task_id":   t.ID,
		"status":    t.Status,
		"total":     t.Total,
		"processed": t.Processed,
		"ok":        t.OK,
		"fail":      t.Fail,
		"warning":   t.Warning,
	}
}

func (t *Task) Final() Event {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.final
}

func (t *Task) publish(event Event) {
	t.mu.Lock()
	queues := append([]chan Event(nil), t.queues...)
	t.mu.Unlock()
	for _, ch := range queues {
		select {
		case ch <- event:
		default:
		}
	}
}

func (t *Task) Record(success bool, item string, detail map[string]any, errText string) {
	t.mu.Lock()
	t.Processed++
	if success {
		t.OK++
	} else {
		t.Fail++
	}
	event := Event{
		"type":      "progress",
		"task_id":   t.ID,
		"total":     t.Total,
		"processed": t.Processed,
		"ok":        t.OK,
		"fail":      t.Fail,
	}
	if item != "" {
		event["item"] = item
	}
	if detail != nil {
		event["detail"] = detail
	}
	if errText != "" {
		event["error"] = errText
	}
	t.mu.Unlock()
	t.publish(event)
}

func (t *Task) Finish(result map[string]any, warning string) {
	t.mu.Lock()
	t.Status = "done"
	t.Warning = warning
	t.Result = result
	event := Event{
		"type":      "done",
		"task_id":   t.ID,
		"total":     t.Total,
		"processed": t.Processed,
		"ok":        t.OK,
		"fail":      t.Fail,
		"warning":   warning,
		"result":    result,
	}
	t.final = event
	t.mu.Unlock()
	t.publish(event)
}

func (t *Task) FailTask(message string) {
	t.mu.Lock()
	t.Status = "error"
	t.Error = message
	event := Event{
		"type":      "error",
		"task_id":   t.ID,
		"total":     t.Total,
		"processed": t.Processed,
		"ok":        t.OK,
		"fail":      t.Fail,
		"error":     message,
	}
	t.final = event
	t.mu.Unlock()
	t.publish(event)
}

func (t *Task) Cancel() {
	t.mu.Lock()
	t.Cancelled = true
	t.mu.Unlock()
}

func (t *Task) FinishCancelled() {
	t.mu.Lock()
	t.Status = "cancelled"
	event := Event{
		"type":      "cancelled",
		"task_id":   t.ID,
		"total":     t.Total,
		"processed": t.Processed,
		"ok":        t.OK,
		"fail":      t.Fail,
	}
	t.final = event
	t.mu.Unlock()
	t.publish(event)
}

type Store struct {
	mu    sync.RWMutex
	tasks map[string]*Task
}

func NewStore() *Store {
	return &Store{tasks: map[string]*Task{}}
}

func (s *Store) Create(total int) *Task {
	task := New(total)
	s.mu.Lock()
	s.tasks[task.ID] = task
	s.mu.Unlock()
	go func() {
		time.Sleep(5 * time.Minute)
		s.mu.Lock()
		delete(s.tasks, task.ID)
		s.mu.Unlock()
	}()
	return task
}

func (s *Store) Get(id string) *Task {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.tasks[id]
}
