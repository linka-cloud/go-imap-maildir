package index

import (
	"bufio"
	"encoding/binary"
	"errors"
	"io"
	"sort"
	"strings"
)

const (
	fileMagic   = "IMDX"
	fileVersion = 1
)

const (
	fileTypeSnapshot = 1
	fileTypeLog      = 2
)

type FieldID uint8

const (
	FieldSubject FieldID = 1
	FieldFrom    FieldID = 2
	FieldTo      FieldID = 3
	FieldCc      FieldID = 4
	FieldDate    FieldID = 5
	FieldMsgID   FieldID = 6
	FieldInReply FieldID = 7
	FieldRefs    FieldID = 8
)

var fieldNames = map[FieldID]string{
	FieldSubject: "Subject",
	FieldFrom:    "From",
	FieldTo:      "To",
	FieldCc:      "Cc",
	FieldDate:    "Date",
	FieldMsgID:   "Message-Id",
	FieldInReply: "In-Reply-To",
	FieldRefs:    "References",
}

var fieldIDs = map[string]FieldID{
	"subject":     FieldSubject,
	"from":        FieldFrom,
	"to":          FieldTo,
	"cc":          FieldCc,
	"date":        FieldDate,
	"message-id":  FieldMsgID,
	"in-reply-to": FieldInReply,
	"references":  FieldRefs,
}

func FieldName(id FieldID) string {
	return fieldNames[id]
}

func FieldIDFromName(name string) (FieldID, bool) {
	id, ok := fieldIDs[strings.ToLower(name)]
	return id, ok
}

type Entry struct {
	Key          string
	UID          uint32
	InternalDate int64
	Size         uint32
	Headers      map[FieldID][]string
}

func (e *Entry) Values(id FieldID) []string {
	if e == nil || e.Headers == nil {
		return nil
	}
	return e.Headers[id]
}

type Index struct {
	GUID    string
	entries map[string]*Entry
}

func New(guid string) *Index {
	return &Index{GUID: guid, entries: map[string]*Entry{}}
}

func (i *Index) Get(key string) (*Entry, bool) {
	if i == nil {
		return nil, false
	}
	entry, ok := i.entries[key]
	return entry, ok
}

func (i *Index) Upsert(entry *Entry) {
	if i == nil || entry == nil {
		return
	}
	if i.entries == nil {
		i.entries = map[string]*Entry{}
	}
	i.entries[entry.Key] = entry
}

func (i *Index) Delete(key string) {
	if i == nil {
		return
	}
	delete(i.entries, key)
}

func (i *Index) Entries() []*Entry {
	if i == nil || len(i.entries) == 0 {
		return nil
	}
	entries := make([]*Entry, 0, len(i.entries))
	keys := make([]string, 0, len(i.entries))
	for key := range i.entries {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		entries = append(entries, i.entries[key])
	}
	return entries
}

type RecordType uint8

const (
	RecordUpsert RecordType = 1
	RecordDelete RecordType = 2
)

type Record struct {
	Type  RecordType
	Entry *Entry
	Key   string
}

func UpsertRecord(entry *Entry) Record {
	return Record{Type: RecordUpsert, Entry: entry}
}

func DeleteRecord(key string) Record {
	return Record{Type: RecordDelete, Key: key}
}

type FileHeader struct {
	Version  uint8
	FileType uint8
	GUID     string
}

func WriteFileHeader(w io.Writer, fileType uint8, guid string) error {
	if _, err := w.Write([]byte(fileMagic)); err != nil {
		return err
	}
	if _, err := w.Write([]byte{fileVersion, fileType}); err != nil {
		return err
	}
	if err := writeUvarint(w, uint64(len(guid))); err != nil {
		return err
	}
	if guid == "" {
		return nil
	}
	_, err := w.Write([]byte(guid))
	return err
}

func ReadFileHeader(r *bufio.Reader) (FileHeader, error) {
	var header FileHeader
	magic := make([]byte, len(fileMagic))
	if _, err := io.ReadFull(r, magic); err != nil {
		return header, err
	}
	if string(magic) != fileMagic {
		return header, errors.New("maildir index: invalid magic")
	}
	ver, err := r.ReadByte()
	if err != nil {
		return header, err
	}
	fileType, err := r.ReadByte()
	if err != nil {
		return header, err
	}
	guidLen, err := readUvarint(r)
	if err != nil {
		return header, err
	}
	guid := ""
	if guidLen > 0 {
		buf := make([]byte, guidLen)
		if _, err := io.ReadFull(r, buf); err != nil {
			return header, err
		}
		guid = string(buf)
	}
	header = FileHeader{Version: ver, FileType: fileType, GUID: guid}
	return header, nil
}

func WriteRecord(w io.Writer, record Record) error {
	if _, err := w.Write([]byte{byte(record.Type)}); err != nil {
		return err
	}
	key := record.Key
	if record.Type == RecordUpsert && record.Entry != nil {
		key = record.Entry.Key
	}
	if err := writeUvarint(w, uint64(len(key))); err != nil {
		return err
	}
	if _, err := w.Write([]byte(key)); err != nil {
		return err
	}
	if record.Type != RecordUpsert {
		return nil
	}
	entry := record.Entry
	if entry == nil {
		return errors.New("maildir index: missing entry")
	}
	if err := writeUvarint(w, uint64(entry.UID)); err != nil {
		return err
	}
	if err := writeVarint(w, entry.InternalDate); err != nil {
		return err
	}
	if err := writeUvarint(w, uint64(entry.Size)); err != nil {
		return err
	}
	if entry.Headers == nil {
		return writeUvarint(w, 0)
	}
	ids := make([]int, 0, len(entry.Headers))
	for id := range entry.Headers {
		ids = append(ids, int(id))
	}
	sort.Ints(ids)
	if err := writeUvarint(w, uint64(len(ids))); err != nil {
		return err
	}
	for _, idVal := range ids {
		id := FieldID(idVal)
		if _, err := w.Write([]byte{byte(id)}); err != nil {
			return err
		}
		values := entry.Headers[id]
		if err := writeUvarint(w, uint64(len(values))); err != nil {
			return err
		}
		for _, value := range values {
			if err := writeUvarint(w, uint64(len(value))); err != nil {
				return err
			}
			if _, err := w.Write([]byte(value)); err != nil {
				return err
			}
		}
	}
	return nil
}

func ReadRecords(r *bufio.Reader, apply func(Record) error) error {
	for {
		typeByte, err := r.ReadByte()
		if err != nil {
			if errors.Is(err, io.EOF) {
				return nil
			}
			return err
		}
		recordType := RecordType(typeByte)
		keyLen, err := readUvarint(r)
		if err != nil {
			return err
		}
		key := ""
		if keyLen > 0 {
			buf := make([]byte, keyLen)
			if _, err := io.ReadFull(r, buf); err != nil {
				return err
			}
			key = string(buf)
		}
		record := Record{Type: recordType, Key: key}
		if recordType == RecordUpsert {
			uid, err := readUvarint(r)
			if err != nil {
				return err
			}
			internalDate, err := readVarint(r)
			if err != nil {
				return err
			}
			size, err := readUvarint(r)
			if err != nil {
				return err
			}
			headerCount, err := readUvarint(r)
			if err != nil {
				return err
			}
			headers := map[FieldID][]string{}
			for i := uint64(0); i < headerCount; i++ {
				idByte, err := r.ReadByte()
				if err != nil {
					return err
				}
				valueCount, err := readUvarint(r)
				if err != nil {
					return err
				}
				values := make([]string, 0, valueCount)
				for j := uint64(0); j < valueCount; j++ {
					valLen, err := readUvarint(r)
					if err != nil {
						return err
					}
					val := ""
					if valLen > 0 {
						buf := make([]byte, valLen)
						if _, err := io.ReadFull(r, buf); err != nil {
							return err
						}
						val = string(buf)
					}
					values = append(values, val)
				}
				headers[FieldID(idByte)] = values
			}
			record.Entry = &Entry{
				Key:          key,
				UID:          uint32(uid),
				InternalDate: internalDate,
				Size:         uint32(size),
				Headers:      headers,
			}
		}
		if apply != nil {
			if err := apply(record); err != nil {
				return err
			}
		}
	}
}

func writeUvarint(w io.Writer, v uint64) error {
	var buf [binary.MaxVarintLen64]byte
	n := binary.PutUvarint(buf[:], v)
	_, err := w.Write(buf[:n])
	return err
}

func writeVarint(w io.Writer, v int64) error {
	var buf [binary.MaxVarintLen64]byte
	n := binary.PutVarint(buf[:], v)
	_, err := w.Write(buf[:n])
	return err
}

func readUvarint(r *bufio.Reader) (uint64, error) {
	return binary.ReadUvarint(r)
}

func readVarint(r *bufio.Reader) (int64, error) {
	return binary.ReadVarint(r)
}
