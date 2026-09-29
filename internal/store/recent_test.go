package store

import (
	"context"
	"fmt"
	"testing"
)

func TestRecentFirstIncludesLatestNoteBeyondPriorityPage(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	project := createRecord(t, s, CreateInput{Kind: "project", Title: "Handover project"})
	for i := 0; i < 105; i++ {
		createRecord(t, s, CreateInput{Kind: "note", Title: fmt.Sprintf("Older high-priority note %d", i), ProjectID: project.ID, Priority: 5})
	}
	latest := createRecord(t, s, CreateInput{Kind: "note", Title: "Latest handover", ProjectID: project.ID, Priority: 0})
	archived := createRecord(t, s, CreateInput{Kind: "note", Title: "Later but archived", ProjectID: project.ID, Priority: 5})
	if _, _, err := s.Update(ctx, archived.ID, "agent-a", "", UpdateInput{Version: 1, Archived: pointer(true)}); err != nil {
		t.Fatal(err)
	}
	options := ListOptions{Kind: "note", ProjectID: project.ID, Limit: 100, RecentFirst: true}
	result, err := s.List(ctx, options)
	if err != nil || result.Total != 106 || len(result.Items) != 100 || result.Items[0].ID != latest.ID {
		t.Fatalf("recent notes omitted the latest handover: %+v %v", result, err)
	}
	for _, item := range result.Items {
		if item.ID == archived.ID {
			t.Fatal("recent notes included an archived handover")
		}
	}
	options.RecentFirst = false
	priorityResult, err := s.List(ctx, options)
	if err != nil || priorityResult.Total != 106 || len(priorityResult.Items) != 100 {
		t.Fatalf("normal list failed: %+v %v", priorityResult, err)
	}
	for _, item := range priorityResult.Items {
		if item.ID == latest.ID || item.Priority != 5 {
			t.Fatal("RecentFirst changed default priority ordering")
		}
	}
}
