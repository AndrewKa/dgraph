/*
 * Copyright 2020 Dgraph Labs, Inc. and Contributors
 *
 * Licensed under the Apache License, Version 2.0 (the "License");
 * you may not use this file except in compliance with the License.
 * You may obtain a copy of the License at
 *
 *     http://www.apache.org/licenses/LICENSE-2.0
 *
 * Unless required by applicable law or agreed to in writing, software
 * distributed under the License is distributed on an "AS IS" BASIS,
 * WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
 * See the License for the specific language governing permissions and
 * limitations under the License.
 */

package raftwal

import (
	"encoding/binary"
	"fmt"
	"os"
	"path"
	"sort"
	"strconv"
	"strings"

	"github.com/dgraph-io/dgraph/x"
	"github.com/dgraph-io/ristretto/z"
	"github.com/golang/glog"
	"github.com/pkg/errors"
	"go.etcd.io/etcd/raft"
	"go.etcd.io/etcd/raft/raftpb"
)

const (
	// entryFileOffset
	entryFileOffset = 1 << 20 // 1MB
	// entryFileSize is the initial size of the entry file.
	entryFileSize = 16 << 30
	// entrySize is the size in bytes of a single entry.
	entrySize = 32
)

type entry []byte

func (e entry) Term() uint64 {
	return binary.BigEndian.Uint64(e)
}
func (e entry) Index() uint64 {
	return binary.BigEndian.Uint64(e[8:])
}
func (e entry) DataOffset() uint64 {
	return binary.BigEndian.Uint64(e[16:])
}
func (e entry) Type() uint64 {
	return binary.BigEndian.Uint64(e[24:])
}

func marshalEntry(b []byte, term, index, do, typ uint64) {
	x.AssertTrue(len(b) == entrySize)

	binary.BigEndian.PutUint64(b, term)
	binary.BigEndian.PutUint64(b[8:], index)
	binary.BigEndian.PutUint64(b[16:], do)
	binary.BigEndian.PutUint64(b[24:], typ)
}

var (
	emptyEntry = entry(make([]byte, entrySize))
)

// entryLog represents the entire entry log. It consists of one or more
// entryFile objects.
type entryLog struct {
	// need lock for files and current ?

	// files is the list of all log files ordered in ascending order by the first
	// index in the file. The current file being written should always be accessible
	// by looking at the last element of this slice.
	files   []*entryFile
	current *entryFile
	// nextEntryIdx is the index of the next entry to write to. When this value exceeds
	// maxNumEntries the file will be rotated.
	nextEntryIdx int
	// dir is the directory to use to store files.
	dir string
}

func entryFileName(dir string, id int64) string {
	return path.Join(dir, fmt.Sprintf("%05d.ent", id))
}

func openEntryLog(dir string) (*entryLog, error) {
	e := &entryLog{
		dir: dir,
	}
	files, err := getEntryFiles(dir)
	if err != nil {
		return nil, err
	}
	out := files[:0]
	var nextFid int64
	for _, ef := range files {
		if nextFid < ef.fid {
			nextFid = ef.fid
		}
		if ef.firstIndex() == 0 {
			if err := ef.delete(); err != nil {
				return nil, err
			}
		} else {
			out = append(out, ef)
		}
	}
	e.files = out
	if sz := len(e.files); sz > 0 {
		e.current = e.files[sz-1]
		e.nextEntryIdx = e.current.firstEmptySlot()

		e.files = e.files[:sz-1]
		return e, nil
	}

	// No files found. Create a new file.
	nextFid += 1
	ef, err := openEntryFile(dir, nextFid)
	e.current = ef
	return e, err
}

func (l *entryLog) lastFile() *entryFile {
	return l.files[len(l.files)-1]
}

// getEntry gets the nth entry in the CURRENT log file.
func (l *entryLog) getEntry(n int) (entry, error) {
	if n >= maxNumEntries {
		return nil, errors.Errorf("there cannot be more than %d in a single file",
			maxNumEntries)
	}

	start := n * entrySize
	buf := l.current.data[start : start+entrySize]
	return entry(buf), nil
}

func (l *entryLog) rotate(firstIndex uint64) error {
	nextFid := l.current.fid
	x.AssertTrue(nextFid > 0)

	for _, ef := range l.files {
		if ef.fid > nextFid {
			nextFid = ef.fid
		}
	}

	nextFid += 1
	ef, err := openEntryFile(l.dir, nextFid)
	if err != nil {
		return errors.Wrapf(err, "while creating a new entry file")
	}

	l.files = append(l.files, l.current)
	l.current = ef
	return nil
}

func (l *entryLog) numEntries() int {
	if len(l.files) == 0 {
		return 0
	}
	total := 0
	if len(l.files) >= 1 {
		// all files except the last one.
		total += (len(l.files) - 1) * maxNumEntries
	}
	return total + l.nextEntryIdx
}

func (l *entryLog) AddEntries(entries []raftpb.Entry) error {
	if len(entries) == 0 {
		return nil
	}
	// glog.Infof("AddEntries: %+v\n", entries)
	fidx, eidx := l.slotGe(entries[0].Index)

	// fmt.Printf("fidx: %d, eidx: %d, entries: %+v\n", fidx, eidx, entries)
	if eidx >= 0 {
		// fmt.Printf("AddEntries: fidx: %d, eidx: %d num: %d\n", fidx, eidx, len(entries))

		if fidx == -1 {
			if l.nextEntryIdx > eidx {
				zeroOut(l.current.data, entrySize*eidx, entrySize*l.nextEntryIdx)
			}
		} else {
			x.AssertTrue(fidx < len(l.files))
			extra := l.files[fidx+1:]
			extra = append(extra, l.current)
			l.current = l.files[fidx]

			for _, ef := range extra {
				glog.V(2).Infof("Deleting extra file: %d\n", ef.fid)
				if err := ef.delete(); err != nil {
					glog.Errorf("deleting file: %s. error: %v\n", ef.fd.Name(), err)
				}
			}
			zeroOut(l.current.data, entrySize*eidx, entryFileOffset)
			l.files = l.files[:fidx]
		}
		l.nextEntryIdx = eidx
	}

	prev := l.nextEntryIdx - 1
	var offset int
	if prev >= 0 {
		e := l.current.getEntry(prev)
		offset = int(e.DataOffset())
		offset += sliceSize(l.current.data, offset)
	} else {
		offset = entryFileOffset
	}

	for _, re := range entries {
		if l.nextEntryIdx >= maxNumEntries {
			if err := l.rotate(re.Index); err != nil {
				// TODO: see what happens.
				return err
			}
			l.nextEntryIdx, offset = 0, entryFileOffset
		}

		destBuf, next := l.current.allocateSlice(len(re.Data), offset)
		x.AssertTrue(copy(destBuf, re.Data) == len(re.Data))

		buf, err := l.getEntry(l.nextEntryIdx)
		x.Check(err)
		marshalEntry(buf, re.Term, re.Index, uint64(offset), uint64(re.Type))

		// Update for next entry.
		offset = next
		l.nextEntryIdx++
	}
	return nil
}

func (l *entryLog) firstIndex() uint64 {
	if l == nil {
		return 0
	}
	var fi uint64
	if len(l.files) == 0 {
		fi = l.current.firstEntry().Index()
	} else {
		fi = l.files[0].firstEntry().Index()
	}
	if fi == 0 {
		return 1
	}
	return fi
}

func (l *entryLog) LastIndex() uint64 {
	if l.nextEntryIdx-1 >= 0 {
		e := l.current.getEntry(l.nextEntryIdx - 1)
		return e.Index()
	}
	for i := len(l.files) - 1; i >= 0; i-- {
		ef := l.files[i]
		e := ef.lastEntry()
		if e.Index() > 0 {
			return e.Index()
		}
	}
	return 0
}

func (l *entryLog) getEntryFile(fidx int) *entryFile {
	if fidx == -1 {
		return l.current
	}
	if fidx >= len(l.files) {
		return nil
	}
	return l.files[fidx]
}

func (l *entryLog) seekEntry(raftIndex uint64) (entry, error) {
	if raftIndex == 0 {
		return emptyEntry, nil
	}

	fidx, off := l.slotGe(raftIndex)
	if off == -1 {
		return emptyEntry, raft.ErrCompacted
	} else if off >= maxNumEntries {
		return emptyEntry, raft.ErrUnavailable
	}

	ef := l.getEntryFile(fidx)
	ent := ef.getEntry(off)
	if ent.Index() == 0 {
		// We have gone past what we wrote to the file.
		return emptyEntry, raft.ErrUnavailable
	}
	if ent.Index() != raftIndex {
		return emptyEntry, errNotFound
	}
	return ent, nil
}

// Term returns the term of entry i, which must be in the range
// [FirstIndex()-1, LastIndex()]. The term of the entry before
// FirstIndex is retained for matching purposes even though the
// rest of that entry may not be available.
func (l *entryLog) Term(idx uint64) (uint64, error) {
	// Look at the entry files and find the entry file with entry bigger than idx.
	// Read file before that idx.
	ent, err := l.seekEntry(idx)
	return ent.Term(), err
}

// slotGe returns the file index and the slot within that file in which the entry
// with the given index can be found. A value of -1 for the file index means that the
// entry is in the current file.
func (l *entryLog) slotGe(raftIndex uint64) (int, int) {
	// Look for the offset in the current file.
	if offset := l.current.slotGe(raftIndex); offset >= 0 {
		return -1, offset
	}

	// No previous files, therefore we can only go back to the start of the current file.
	if len(l.files) == 0 {
		return -1, -1
	}

	fileIdx := sort.Search(len(l.files), func(i int) bool {
		return l.files[i].firstIndex() >= raftIndex
	})
	if fileIdx >= len(l.files) {
		fileIdx = len(l.files) - 1
	}
	for fileIdx > 0 {
		fi := l.files[fileIdx].firstIndex()
		if fi <= raftIndex {
			break
		}
		fileIdx--
	}
	offset := l.files[fileIdx].slotGe(raftIndex)
	return fileIdx, offset
}

func (l *entryLog) deleteBefore(raftIndex uint64) error {
	fidx, off := l.slotGe(raftIndex)

	if off >= 0 && fidx <= len(l.files) {
		var before []*entryFile
		if fidx == -1 { // current file
			before = l.files
			l.files = l.files[:0]
		} else {
			before = l.files[:fidx]
			l.files = l.files[fidx:]
		}

		for _, ef := range before {
			if err := ef.delete(); err != nil {
				glog.Errorf("while deleting file: %s, err: %v\n", ef.fd.Name(), err)
			}
		}
	}
	return nil
}

func (l *entryLog) reset() error {
	for _, ef := range l.files {
		if err := ef.delete(); err != nil {
			return errors.Wrapf(err, "while deleting %s", ef.fd.Name())
		}
	}
	l.files = l.files[:0]
	zeroOut(l.current.data, 0, entryFileOffset)
	var num int
	for _, b := range l.current.data[:entryFileOffset] {
		x.AssertTrue(b == 0x00)
		num++
	}
	l.nextEntryIdx = 0
	return nil
}

func (l *entryLog) allEntries(lo, hi, maxSize uint64) []raftpb.Entry {
	var entries []raftpb.Entry
	fileIdx, offset := l.slotGe(lo)
	var size uint64

	if offset < 0 {
		// We are at the very beginning of this thing.
		offset = 0
	}

	currFile := l.getEntryFile(fileIdx)
	for {
		if offset >= maxNumEntries {
			if fileIdx == -1 {
				// We are looking at the current file and there are no more entries.
				// Return what we have.
				return entries
			}

			// Move to the next file.
			fileIdx++
			if fileIdx >= len(l.files) {
				fileIdx = -1
			}
			currFile = l.getEntryFile(fileIdx)
			x.AssertTrue(currFile != nil)

			// Reset the offset to start reading the next file from the beginning.
			offset = 0
		}

		re := currFile.GetRaftEntry(offset)
		if re.Index >= hi {
			return entries
		}
		if re.Index == 0 {
			// Allow this to move to the next file.
			offset = maxNumEntries
			continue
		}
		size += uint64(re.Size())
		if len(entries) > 0 && size > maxSize {
			break
		}
		entries = append(entries, re)
		offset++
	}
	return entries
}

// entryFile represents a single log file.
type entryFile struct {
	*mmapFile
	fid int64
}

func openEntryFile(dir string, fid int64) (*entryFile, error) {
	glog.V(2).Infof("opening entry file: %d\n", fid)
	fpath := entryFileName(dir, fid)
	mf, err := openMmapFile(fpath, os.O_RDWR|os.O_CREATE, entryFileSize)

	if err == errNewFile {
		glog.V(2).Infof("New file: %d\n", fid)
		zeroOut(mf.data, 0, entryFileOffset)
	} else {
		x.Check(err)
	}

	ef := &entryFile{
		mmapFile: mf,
		fid:      fid,
	}
	return ef, nil
}

func getEntryFiles(dir string) ([]*entryFile, error) {
	entryFiles := x.WalkPathFunc(dir, func(path string, isDir bool) bool {
		if isDir {
			return false
		}
		if strings.HasSuffix(path, ".ent") {
			return true
		}
		return false
	})

	var files []*entryFile
	seen := make(map[int64]struct{})
	for _, fpath := range entryFiles {
		_, fname := path.Split(fpath)
		fname = strings.TrimSuffix(fname, ".ent")
		fid, err := strconv.ParseInt(fname, 10, 64)
		if err != nil {
			return nil, errors.Wrapf(err, "while parsing: %s", fpath)
		}
		if _, ok := seen[fid]; ok {
			glog.Fatalf("Entry file with id: %d is repeated", fid)
		}
		seen[fid] = struct{}{}

		f, err := openEntryFile(dir, fid)
		if err != nil {
			return nil, err
		}
		glog.Infof("Found file: %d First Index: %d\n", fid, f.firstIndex())
		files = append(files, f)
	}

	// Sort files by the first index they store.
	sort.Slice(files, func(i, j int) bool {
		return files[i].firstEntry().Index() < files[j].firstEntry().Index()
	})
	return files, nil
}

// get entry from a file.
func (ef *entryFile) getEntry(idx int) entry {
	if ef == nil {
		return emptyEntry
	}
	offset := idx * entrySize
	return entry(ef.data[offset : offset+entrySize])
}

func (ef *entryFile) GetRaftEntry(idx int) raftpb.Entry {
	entry := ef.getEntry(idx)
	re := raftpb.Entry{
		Term:  entry.Term(),
		Index: entry.Index(),
		Type:  raftpb.EntryType(int32(entry.Type())),
	}
	if entry.DataOffset() > 0 && entry.DataOffset() < entryFileSize {
		data := ef.slice(int(entry.DataOffset()))
		if len(data) > 0 {
			re.Data = append(re.Data, data...)
		}
	}
	return re
}

func (ef *entryFile) firstEntry() entry {
	return ef.getEntry(0)
}
func (ef *entryFile) firstIndex() uint64 {
	return ef.getEntry(0).Index()
}
func (ef *entryFile) firstEmptySlot() int {
	return sort.Search(maxNumEntries, func(i int) bool {
		e := ef.getEntry(i)
		return e.Index() == 0
	})
}
func (ef *entryFile) lastEntry() entry {
	// This would return the first pos, where e.Index() == 0.
	pos := ef.firstEmptySlot()
	if pos > 0 {
		pos--
	}
	return ef.getEntry(pos)
}

func (ef *entryFile) Term(entryIndex uint64) uint64 {
	offset := ef.slotGe(entryIndex)
	if offset < 0 || offset >= maxNumEntries {
		return 0
	}
	e := ef.getEntry(int(offset))
	if e.Index() == entryIndex {
		return e.Term()
	}
	return 0
}

// slotGe would return -1 if raftIndex < firstIndex in this file.
// Would return maxNumEntries if raftIndex > lastIndex in this file.
// If raftIndex is found, or the entryFile has empty slots, the offset would be between
// [0, maxNumEntries).
func (ef *entryFile) slotGe(raftIndex uint64) int {
	fi := ef.firstIndex()
	// If first index is zero, this raftindex should be in a previous file.
	if fi == 0 {
		return -1
	}
	if raftIndex < fi {
		return -1
	}
	if diff := int(raftIndex - fi); diff < maxNumEntries && diff >= 0 {
		e := ef.getEntry(diff)
		if e.Index() == raftIndex {
			return diff
		}
	}

	// This would find the first entry's index which has entryIndex.
	return sort.Search(maxNumEntries, func(i int) bool {
		e := ef.getEntry(i)
		if e.Index() == 0 {
			// We reached too far to the right.
			return true
		}
		return e.Index() >= raftIndex
	})
}

func (ef *entryFile) delete() error {
	glog.V(2).Infof("Deleting file: %s\n", ef.fd.Name())
	if err := z.Munmap(ef.data); err != nil {
		glog.Errorf("while munmap file: %s, error: %v\n", ef.fd.Name(), err)
	}
	if err := ef.fd.Truncate(0); err != nil {
		glog.Errorf("while truncate file: %s, error: %v\n", ef.fd.Name(), err)
	}
	return os.Remove(ef.fd.Name())
}