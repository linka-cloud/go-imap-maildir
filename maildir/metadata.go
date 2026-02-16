package maildir

import (
	"crypto/rand"
	"fmt"
	"time"
)

type Metadata struct {
	Loaded bool

	UIDValidity uint32
	UIDNext     uint32
	GUID        string

	UIDByKey      map[string]uint32
	FilenameByUID map[uint32]string

	KeywordByName map[string]rune
	NameByKeyword map[rune]string

	DirtyUIDList  bool
	DirtyKeywords bool

	uidlistName  string
	keywordsName string
}

func (m *Metadata) Ensure() {
	if m.UIDByKey == nil {
		m.UIDByKey = map[string]uint32{}
	}
	if m.FilenameByUID == nil {
		m.FilenameByUID = map[uint32]string{}
	}
	if m.KeywordByName == nil {
		m.KeywordByName = map[string]rune{}
	}
	if m.NameByKeyword == nil {
		m.NameByKeyword = map[rune]string{}
	}
}

func (m *Metadata) EnsureKeyword(keyword string) rune {
	if letter, ok := m.KeywordByName[keyword]; ok {
		return letter
	}
	for i := range 26 {
		letter := rune('a' + i)
		if _, ok := m.NameByKeyword[letter]; !ok {
			m.KeywordByName[keyword] = letter
			m.NameByKeyword[letter] = keyword
			m.DirtyKeywords = true
			return letter
		}
	}
	return 0
}

func (m *Metadata) EnsureGUID() {
	if m.GUID == "" {
		m.GUID = newMailboxGUID()
		m.DirtyUIDList = true
	}
}

func (m *Metadata) UIDListName() string {
	return m.uidlistName
}

func (m *Metadata) SetUIDListName(name string) {
	m.uidlistName = name
}

func (m *Metadata) KeywordsName() string {
	return m.keywordsName
}

func (m *Metadata) SetKeywordsName(name string) {
	m.keywordsName = name
}

func newMailboxGUID() string {
	buf := make([]byte, 16)
	if _, err := rand.Read(buf); err != nil {
		return fmt.Sprintf("%x", time.Now().UnixNano())
	}
	return fmt.Sprintf("%x", buf)
}
