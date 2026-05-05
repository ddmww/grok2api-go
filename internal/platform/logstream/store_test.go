package logstream

import (
	"testing"
	"time"
)

func TestStoreCapacityFilterAndClear(t *testing.T) {
	store := NewStore(2)
	store.Add(Event{Category: CategoryChat, Level: LevelInfo, Message: "one"})
	store.Add(Event{Category: CategoryImage, Level: LevelWarn, Message: "two"})
	store.Add(Event{Category: CategoryError, Level: LevelError, Message: "three"})

	all := store.List(Query{Limit: 10})
	if len(all) != 2 {
		t.Fatalf("len(all) = %d, want 2", len(all))
	}
	if all[0].Message != "three" || all[1].Message != "two" {
		t.Fatalf("unexpected order: %#v", all)
	}

	errors := store.List(Query{Level: LevelError, Limit: 10})
	if len(errors) != 1 || errors[0].Category != CategoryError {
		t.Fatalf("unexpected error filter: %#v", errors)
	}

	images := store.List(Query{Category: CategoryImage, Limit: 10})
	if len(images) != 1 || images[0].Message != "two" {
		t.Fatalf("unexpected category filter: %#v", images)
	}

	store.Clear()
	if got := store.List(Query{Limit: 10}); len(got) != 0 {
		t.Fatalf("len(after clear) = %d, want 0", len(got))
	}
}

func TestSubscribeReceivesMatchingEvents(t *testing.T) {
	store := NewStore(10)
	events, cancel := store.Subscribe(Query{Category: CategoryChat})
	defer cancel()

	store.Add(Event{Category: CategoryImage, Level: LevelInfo, Message: "skip"})
	store.Add(Event{Category: CategoryChat, Level: LevelInfo, Message: "keep"})

	select {
	case event := <-events:
		if event.Message != "keep" {
			t.Fatalf("event.Message = %q, want keep", event.Message)
		}
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for subscribed event")
	}
}

func TestMaskSSO(t *testing.T) {
	if got := MaskSSO("sso=abcdefghijklmnopqrstuvwxyz"); got != "abcdef...wxyz" {
		t.Fatalf("MaskSSO() = %q", got)
	}
	if got := MaskSSO("short"); got != "shor****" {
		t.Fatalf("MaskSSO(short) = %q", got)
	}
}
