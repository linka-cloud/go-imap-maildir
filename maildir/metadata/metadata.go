package metadata

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/foxcpp/go-imap-maildir/maildir"
)

const (
	UIDListFileName     = "uidlist"
	UIDListLockFileName = "uidlist.lock"
	KeywordsFileName    = "keywords"
	AttributesFileName  = "attributes"
)

func ParseUIDList(r io.Reader, state *maildir.Metadata) error {
	reader := bufio.NewReader(r)
	line, err := reader.ReadString('\n')
	if err != nil && !errors.Is(err, io.EOF) {
		return err
	}
	line = strings.TrimSpace(line)
	if line != "" {
		parseUIDListHeader(state, line)
	}

	for {
		line, err = reader.ReadString('\n')
		if err != nil && !errors.Is(err, io.EOF) {
			return err
		}
		line = strings.TrimSpace(line)
		if line != "" {
			parseUIDListEntry(state, line)
		}
		if errors.Is(err, io.EOF) {
			break
		}
	}

	FinalizeUIDList(state)
	return nil
}

func InitUIDList(state *maildir.Metadata) {
	state.UIDValidity = uint32(time.Now().UnixNano())
	if state.UIDValidity == 0 {
		state.UIDValidity = 1
	}
	state.UIDNext = 1
}

func FinalizeUIDList(state *maildir.Metadata) {
	if state.UIDNext == 0 {
		var max uint32
		for _, uid := range state.UIDByKey {
			if uid > max {
				max = uid
			}
		}
		state.UIDNext = max + 1
	}

	if state.UIDValidity == 0 {
		state.UIDValidity = uint32(time.Now().UnixNano())
		if state.UIDValidity == 0 {
			state.UIDValidity = 1
		}
	}
	if state.GUID == "" {
		state.EnsureGUID()
	}
}

func WriteUIDList(w io.Writer, state *maildir.Metadata) error {
	guid := state.GUID
	if guid == "" {
		state.EnsureGUID()
		guid = state.GUID
	}
	if _, err := fmt.Fprintf(w, "3 V%d N%d G%s\n", state.UIDValidity, state.UIDNext, guid); err != nil {
		return err
	}

	uids := make([]uint32, 0, len(state.FilenameByUID))
	for uid := range state.FilenameByUID {
		uids = append(uids, uid)
	}
	slices.Sort(uids)

	for _, uid := range uids {
		name := state.FilenameByUID[uid]
		if name == "" {
			continue
		}
		if _, err := fmt.Fprintf(w, "%d :%s\n", uid, name); err != nil {
			return err
		}
	}

	return nil
}

func ReadKeywords(r io.Reader, state *maildir.Metadata) error {
	reader := bufio.NewReader(r)
	for {
		line, err := reader.ReadString('\n')
		if err != nil && !errors.Is(err, io.EOF) {
			return err
		}
		line = strings.TrimSpace(line)
		if line != "" {
			fields := strings.Fields(line)
			if len(fields) >= 2 {
				idx, err := strconv.Atoi(fields[0])
				if err == nil && idx >= 0 && idx < 26 {
					keyword := strings.Join(fields[1:], " ")
					letter := rune('a' + idx)
					state.KeywordByName[keyword] = letter
					state.NameByKeyword[letter] = keyword
				}
			}
		}
		if errors.Is(err, io.EOF) {
			break
		}
	}

	return nil
}

func WriteKeywords(w io.Writer, state *maildir.Metadata) error {
	letters := make([]rune, 0, len(state.NameByKeyword))
	for letter := range state.NameByKeyword {
		letters = append(letters, letter)
	}
	slices.Sort(letters)

	for _, letter := range letters {
		idx := int(letter - 'a')
		if idx < 0 || idx >= 26 {
			continue
		}
		name := state.NameByKeyword[letter]
		if _, err := fmt.Fprintf(w, "%d %s\n", idx, name); err != nil {
			return err
		}
	}

	return nil
}

func ReadAttributes(r io.Reader) (map[string]string, error) {
	attrs := map[string]string{}
	reader := bufio.NewReader(r)
	for {
		keyLine, err := reader.ReadString('\n')
		if err != nil && !errors.Is(err, io.EOF) {
			return nil, err
		}
		keyLine = strings.TrimRight(keyLine, "\r\n")
		if keyLine == "" && errors.Is(err, io.EOF) {
			break
		}
		valueLine, err2 := reader.ReadString('\n')
		if err2 != nil && !errors.Is(err2, io.EOF) {
			return nil, err2
		}
		valueLine = strings.TrimRight(valueLine, "\r\n")
		key := tabUnescape(keyLine)
		value := tabUnescape(valueLine)
		if key != "" {
			attrs[key] = value
		}
		if errors.Is(err, io.EOF) || errors.Is(err2, io.EOF) {
			break
		}
	}
	return attrs, nil
}

func WriteAttributes(w io.Writer, attrs map[string]string) error {
	keys := make([]string, 0, len(attrs))
	for key := range attrs {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		value := attrs[key]
		if _, err := fmt.Fprintln(w, tabEscape(key)); err != nil {
			return err
		}
		if _, err := fmt.Fprintln(w, tabEscape(value)); err != nil {
			return err
		}
	}
	return nil
}

func ParseMaildirKey(filename string) string {
	key, _, _ := strings.Cut(filename, ":")
	return key
}

func BaseName(value string) string {
	if idx := strings.LastIndex(value, "/"); idx >= 0 {
		return value[idx+1:]
	}
	return value
}

func parseUIDListHeader(state *maildir.Metadata, line string) {
	fields := strings.Fields(line)
	for _, field := range fields {
		switch {
		case strings.HasPrefix(field, "V"):
			if val, err := strconv.ParseUint(field[1:], 10, 32); err == nil {
				state.UIDValidity = uint32(val)
			}
		case strings.HasPrefix(field, "N"):
			if val, err := strconv.ParseUint(field[1:], 10, 32); err == nil {
				state.UIDNext = uint32(val)
			}
		case strings.HasPrefix(field, "G"):
			state.GUID = field[1:]
		}
	}
}

func parseUIDListEntry(state *maildir.Metadata, line string) {
	parts := strings.SplitN(line, ":", 2)
	if len(parts) != 2 {
		return
	}
	left := strings.TrimSpace(parts[0])
	right := strings.TrimSpace(parts[1])
	if left == "" || right == "" {
		return
	}
	fields := strings.Fields(left)
	if len(fields) == 0 {
		return
	}
	uid, err := strconv.ParseUint(fields[0], 10, 32)
	if err != nil || uid == 0 {
		return
	}
	filename := BaseName(right)
	key := ParseMaildirKey(filename)
	if key == "" {
		return
	}
	state.UIDByKey[key] = uint32(uid)
	state.FilenameByUID[uint32(uid)] = filename
}

func tabEscape(value string) string {
	value = strings.ReplaceAll(value, "\\", "\\\\")
	value = strings.ReplaceAll(value, "\t", "\\t")
	value = strings.ReplaceAll(value, "\n", "\\n")
	value = strings.ReplaceAll(value, "\r", "\\r")
	return value
}

func tabUnescape(value string) string {
	value = strings.ReplaceAll(value, "\\t", "\t")
	value = strings.ReplaceAll(value, "\\n", "\n")
	value = strings.ReplaceAll(value, "\\r", "\r")
	value = strings.ReplaceAll(value, "\\\\", "\\")
	return value
}
