package store

import (
	"errors"
	"os"
	"testing"
	"time"
)

// TestCleanupRetry 验证删除失败后保留索引并在恢复后清理。
func TestCleanupRetry(t *testing.T) {
	s, err := New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	d, err := s.Create("test")
	if err != nil {
		t.Fatal(err)
	}
	d.ExpiresAt = time.Now().Add(-time.Second)
	d.State = "submitted"
	d.JobID = "job-1"
	s.Add(d)
	s.AddJob(d)
	s.removeAll = func(string) error { return errors.New("disk failure") }
	if err = s.RemoveFiles(d); err == nil {
		t.Fatal("expected failure")
	}
	s.Cleanup()
	if s.docs[d.ID] != d || !s.pending[d.Dir] {
		t.Fatal("cleanup failure lost tracking")
	}
	if s.Get(d.ID) != nil || s.HasJob(d.JobID) {
		t.Fatal("expired item was visible")
	}
	s.removeAll = os.RemoveAll
	s.Cleanup()
	if _, ok := s.docs[d.ID]; ok {
		t.Fatal("document still tracked")
	}
	if len(s.pending) != 0 {
		t.Fatal("pending cleanup remains")
	}
	if _, err = os.Stat(d.Dir); !os.IsNotExist(err) {
		t.Fatalf("directory exists: %v", err)
	}
}

// TestFailedUploadCleanupRetry 验证未发布文档清理失败后也会重试。
func TestFailedUploadCleanupRetry(t *testing.T) {
	s, err := New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	d, err := s.Create("bad")
	if err != nil {
		t.Fatal(err)
	}
	s.removeAll = func(string) error { return errors.New("failure") }
	_ = s.RemoveFiles(d)
	s.removeAll = os.RemoveAll
	s.Cleanup()
	if len(s.pending) != 0 {
		t.Fatal("pending cleanup remains")
	}
	if _, err = os.Stat(d.Dir); !os.IsNotExist(err) {
		t.Fatalf("directory exists: %v", err)
	}
}
