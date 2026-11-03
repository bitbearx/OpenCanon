// Package store 管理临时文档和打印任务的生命周期。
package store

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"log"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// Document 保存仅在本进程内有效的文档信息。
type Document struct {
	Mu                                               sync.Mutex
	ID, Name, Kind, Path, Preview, Dir, JobID, State string
	PageCount                                        int
	ExpiresAt                                        time.Time
}

// Store 持有文档及任务索引。
type Store struct {
	mu        sync.Mutex
	docs      map[string]*Document
	jobs      map[string]string
	pending   map[string]bool
	removeAll func(string) error
	root      string
}

// New 创建私有文档目录并删除上次运行遗留的内容。
func New(dataDir string) (*Store, error) {
	dataDir, err := filepath.Abs(dataDir)
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(dataDir, 0700); err != nil {
		return nil, err
	}
	if err := os.Chmod(dataDir, 0700); err != nil {
		return nil, err
	}
	root := filepath.Join(dataDir, "documents")
	if err := os.RemoveAll(root); err != nil {
		return nil, err
	}
	if err := os.MkdirAll(root, 0700); err != nil {
		return nil, err
	}
	return &Store{docs: map[string]*Document{}, jobs: map[string]string{}, root: root, pending: map[string]bool{}, removeAll: os.RemoveAll}, nil
}

// Create 创建不暴露路径的随机文档目录。
func (s *Store) Create(name string) (*Document, error) {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return nil, err
	}
	id := hex.EncodeToString(b)
	dir := filepath.Join(s.root, id)
	if err := os.Mkdir(dir, 0700); err != nil {
		return nil, err
	}
	return &Document{ID: id, Name: name, Dir: dir, ExpiresAt: time.Now().Add(time.Hour), State: "ready"}, nil
}

// Add 发布解析成功的文档。
func (s *Store) Add(d *Document) { s.mu.Lock(); defer s.mu.Unlock(); s.docs[d.ID] = d }

// Get 返回尚未过期的文档；调用者应锁定文档后再次检查过期时间。
func (s *Store) Get(id string) *Document {
	s.mu.Lock()
	defer s.mu.Unlock()
	d := s.docs[id]
	if d == nil || time.Now().After(d.ExpiresAt) {
		return nil
	}
	return d
}

// AddJob 建立本进程提交的任务索引。
func (s *Store) AddJob(d *Document) { s.mu.Lock(); defer s.mu.Unlock(); s.jobs[d.JobID] = d.ID }

// HasJob 判断任务是否由本进程提交且尚未过期。
func (s *Store) HasJob(id string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	d := s.docs[s.jobs[id]]
	return d != nil && time.Now().Before(d.ExpiresAt)
}

// Cleanup 删除过期文档并重试删除失败的文件。
func (s *Store) Cleanup() {
	s.mu.Lock()
	docs := make([]*Document, 0, len(s.docs))
	for _, d := range s.docs {
		if time.Now().After(d.ExpiresAt) {
			docs = append(docs, d)
		}
	}
	paths := make([]string, 0, len(s.pending))
	for path := range s.pending {
		paths = append(paths, path)
	}
	s.mu.Unlock()
	for _, path := range paths {
		s.removePath(path)
	}
	for _, d := range docs {
		d.Mu.Lock()
		if time.Now().After(d.ExpiresAt) {
			if err := s.RemoveFiles(d); err == nil {
				s.mu.Lock()
				if s.docs[d.ID] == d {
					delete(s.docs, d.ID)
					if d.JobID != "" {
						delete(s.jobs, d.JobID)
					}
				}
				s.mu.Unlock()
			}
		}
		d.Mu.Unlock()
	}
}

// RemoveFiles 删除文件；失败时登记目录以便下次重试。
func (s *Store) RemoveFiles(d *Document) error {
	if d.Dir == "" {
		return errors.New("missing document directory")
	}
	return s.removePath(d.Dir)
}

// removePath 删除受管理目录并更新重试队列。
func (s *Store) removePath(path string) error {
	err := s.removeAll(path)
	s.mu.Lock()
	if err != nil {
		s.pending[path] = true
	} else {
		delete(s.pending, path)
	}
	s.mu.Unlock()
	if err != nil {
		log.Print("临时文档清理失败，将自动重试")
	}
	return err
}

// RunCleanup 定期清理文档，关闭通道后退出。
func (s *Store) RunCleanup(done <-chan struct{}) {
	ticker := time.NewTicker(5 * time.Minute)
	defer ticker.Stop()
	for {
		select {
		case <-ticker.C:
			s.Cleanup()
		case <-done:
			return
		}
	}
}
