/* Copyright (c) 2014-2015, Daniel Martí <mvdan@mvdan.cc> */
/* See LICENSE for licensing information */

package main

import (
	"bytes"
	"os"
	"sync"
	"syscall"
	"time"
)

type MmapStore struct {
	sync.RWMutex
	cache map[ID]mmapCache

	dir   string
	stats Stats
}

type mmapCache struct {
	reading sync.WaitGroup
	modTime time.Time
	path    string
	mmap    []byte
	size    int64
}

type MmapPaste struct {
	content *bytes.Reader
	cache   *mmapCache
}

func (c MmapPaste) Read(p []byte) (n int, err error) {
	return c.content.Read(p)
}

func (c MmapPaste) ReadAt(p []byte, off int64) (n int, err error) {
	return c.content.ReadAt(p, off)
}

func (c MmapPaste) Seek(offset int64, whence int) (int64, error) {
	return c.content.Seek(offset, whence)
}

func (c MmapPaste) Close() error {
	c.cache.reading.Done()
	return nil
}

func (c MmapPaste) ModTime() time.Time {
	return c.cache.modTime
}

func (c MmapPaste) Size() int64 {
	return c.cache.size
}

func NewMmapStore(dir string) (*MmapStore, error) {
	if err := setupTopDir(dir); err != nil {
		return nil, err
	}
	s := new(MmapStore)
	s.dir = dir
	s.cache = make(map[ID]mmapCache)
	if err := setupSubdirs(s.dir, s.Recover); err != nil {
		return nil, err
	}
	return s, nil
}

func (s *MmapStore) Get(id ID) (Paste, error) {
	s.RLock()
	defer s.RUnlock()
	cached, e := s.cache[id]
	if !e {
		return nil, ErrPasteNotFound
	}
	reader := bytes.NewReader(cached.mmap)
	cached.reading.Add(1)
	return MmapPaste{content: reader, cache: &cached}, nil
}

func (s *MmapStore) Put(content []byte) (ID, error) {
	s.Lock()
	defer s.Unlock()
	size := int64(len(content))
	if err := s.stats.hasSpaceFor(size); err != nil {
		return ID{}, err
	}
	available := func(id ID) bool {
		_, e := s.cache[id]
		return !e
	}
	id, err := randomID(available)
	if err != nil {
		return id, err
	}
	pastePath := pathFromID(id)
	if err = writeNewFile(pastePath, content); err != nil {
		return id, err
	}
	f, err := os.Open(pastePath)
	data, err := getMmap(f, len(content))
	if err != nil {
		return id, err
	}
	s.stats.makeSpaceFor(size)
	s.cache[id] = mmapCache{
		path:    pastePath,
		modTime: time.Now(),
		size:    size,
		mmap:    data,
	}
	return id, nil
}

func (s *MmapStore) Delete(id ID) error {
	s.Lock()
	defer s.Unlock()
	cached, e := s.cache[id]
	if !e {
		return ErrPasteNotFound
	}
	delete(s.cache, id)
	cached.reading.Wait()
	if err := syscall.Munmap(cached.mmap); err != nil {
		return err
	}
	if err := os.Remove(cached.path); err != nil {
		return err
	}
	s.stats.freeSpace(cached.size)
	return nil
}

func (s *MmapStore) Recover(path string, fileInfo os.FileInfo, err error) error {
	if err != nil || fileInfo.IsDir() {
		return err
	}
	id, err := idFromPath(path)
	if err != nil {
		return err
	}
	modTime := fileInfo.ModTime()
	deathTime := modTime.Add(lifeTime)
	lifeLeft := deathTime.Sub(startTime)
	if lifeTime > 0 && lifeLeft <= 0 {
		return os.Remove(path)
	}
	size := fileInfo.Size()
	if size == 0 {
		return os.Remove(path)
	}
	s.Lock()
	defer s.Unlock()
	if err := s.stats.hasSpaceFor(size); err != nil {
		return err
	}
	pasteFile, err := os.Open(path)
	defer pasteFile.Close()
	mmap, err := getMmap(pasteFile, int(fileInfo.Size()))
	if err != nil {
		return err
	}
	s.stats.makeSpaceFor(size)
	cached := mmapCache{
		modTime: modTime,
		path:    path,
		mmap:    mmap,
		size:    size,
	}
	s.cache[id] = cached
	setupPasteDeletion(s, id, lifeLeft)
	return nil
}

func (s *MmapStore) Report() string {
	s.Lock()
	defer s.Unlock()
	return s.stats.Report()
}

func getMmap(file *os.File, length int) ([]byte, error) {
	fd := int(file.Fd())
	return syscall.Mmap(fd, 0, length, syscall.PROT_READ, syscall.MAP_SHARED)
}
