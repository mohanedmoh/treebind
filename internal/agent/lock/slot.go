/*
Copyright The Treebind Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package lock

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"
)

const (
	// Version is the version of the record format. It changes only when a field
	// is removed or changes its meaning; within a version fields are only added,
	// and a reader ignores the ones it does not know.
	Version = 1

	// slotPrefix starts the name of a slot. The number follows in twenty digits,
	// so that every number has one name and a listing sorts by it.
	slotPrefix = "slot-"

	// maxRecord bounds what is read from a slot. A longer file is not a record.
	maxRecord = 4096
)

// record is what a holder writes into a slot it made. It tells people and
// waiting agents who holds the lock. Exclusion does not depend on it: a slot
// counts by existing, whatever it holds.
type record struct {
	// Version is the version of the format, Version.
	Version int `json:"version"`
	// Epoch is the number of the slot the holder's tenure began with.
	Epoch uint64 `json:"epoch"`
	// Holder is the holder's name.
	Holder string `json:"holder"`
	// Time is the holder's wall clock when it made the slot. It is there for
	// people. No decision reads it, because that clock may be wrong.
	Time time.Time `json:"time"`
	// Released is true in the last slot of a holder that gave the lock up.
	Released bool `json:"released"`
}

// slotName is the name of the file of slot n.
func slotName(n uint64) string {
	return fmt.Sprintf("%s%020d", slotPrefix, n)
}

// slotPath is the file of slot n.
func slotPath(dir string, n uint64) string {
	return filepath.Join(dir, slotName(n))
}

// slotNumber is the number of the slot a file name stands for. A name that is
// not exactly the one slotName gives a number is not a slot, so that no number
// has two names.
func slotNumber(name string) (uint64, bool) {
	n, err := strconv.ParseUint(strings.TrimPrefix(name, slotPrefix), 10, 64)
	if err != nil || n == 0 || slotName(n) != name {
		return 0, false
	}
	return n, true
}

// listSlots returns the numbers of the slots in the directory. Other files are
// ignored and left alone.
func listSlots(dir string) ([]uint64, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, fmt.Errorf("lock: list the lock directory: %w", err)
	}
	slots := make([]uint64, 0, len(entries))
	for _, entry := range entries {
		if n, ok := slotNumber(entry.Name()); ok {
			slots = append(slots, n)
		}
	}
	return slots, nil
}

// highest is the highest of the slots, or zero when there is none.
func highest(slots []uint64) uint64 {
	if len(slots) == 0 {
		return 0
	}
	return slices.Max(slots)
}

// makeSlot makes slot n with an exclusive create, which only one agent can win,
// and writes the record into it. made says whether the slot is this agent's now:
// it is even when the record could not be written, and then the error says why.
// When the slot exists already, the error is fs.ErrExist.
//
// The slot and its directory entry are synced, so that a slot an agent relied on
// is not undone by a crash of the storage. Only its maker may write to a slot,
// because nobody ever changes one.
func makeSlot(dir string, n uint64, rec record) (made bool, err error) {
	data, err := json.Marshal(rec)
	if err != nil {
		return false, fmt.Errorf("lock: encode the record: %w", err)
	}
	file, err := os.OpenFile(slotPath(dir, n), os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644)
	if err != nil {
		return false, fmt.Errorf("lock: make slot %d: %w", n, err)
	}
	_, err = file.Write(append(data, '\n'))
	if err == nil {
		err = file.Sync()
	}
	if closeErr := file.Close(); err == nil {
		err = closeErr
	}
	if err == nil {
		err = syncDir(dir)
	}
	if err != nil {
		return true, fmt.Errorf("lock: write the record of slot %d: %w", n, err)
	}
	return true, nil
}

// syncDir makes the entries of the directory durable.
func syncDir(dir string) error {
	handle, err := os.Open(dir)
	if err != nil {
		return err
	}
	err = handle.Sync()
	if closeErr := handle.Close(); err == nil {
		err = closeErr
	}
	return err
}

// readRecord reads the record of slot n. It fails for a slot that is empty,
// torn, not a record or gone, which a reader has to expect: a slot exists before
// its record is written, and a holder may die in between. Of a record in a newer
// format only the version is returned.
func readRecord(dir string, n uint64) (record, error) {
	file, err := os.Open(slotPath(dir, n))
	if err != nil {
		return record{}, err
	}
	data, err := io.ReadAll(io.LimitReader(file, maxRecord+1))
	if closeErr := file.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		return record{}, err
	}
	if len(data) > maxRecord {
		return record{}, errors.New("lock: the slot holds more than a record")
	}
	// The version is read by itself first: the other fields of a newer format may
	// not be the ones this agent knows.
	var head struct {
		Version int `json:"version"`
	}
	if err := json.Unmarshal(data, &head); err != nil {
		return record{}, fmt.Errorf("lock: the slot holds no record: %w", err)
	}
	rec := record{Version: head.Version}
	switch {
	case head.Version < 1:
		return record{}, errors.New("lock: the record has no format version")
	case head.Version > Version:
		return rec, nil
	}
	if err := json.Unmarshal(data, &rec); err != nil {
		return record{}, fmt.Errorf("lock: the slot holds no record: %w", err)
	}
	return rec, nil
}
