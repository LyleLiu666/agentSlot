package session

import (
	"bytes"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sort"
	"sync/atomic"
	"time"

	agent "github.com/LyleLiu666/agentSlot/agent"
)

const sessionListCursorVersion = 1

var sessionListFallbackIdentity atomic.Uint64

type sessionListIndex struct {
	epoch         string
	cursorKey     [sha256.Size]byte
	generation    uint64
	created       map[agent.SessionID]uint64
	lastUpdatedAt time.Time
}

type listedSession struct {
	summary    SessionSummary
	generation uint64
}

type sessionListCursor struct {
	Version        int               `json:"version"`
	Epoch          string            `json:"epoch"`
	AgentID        agent.AgentID     `json:"agent_id"`
	WorkspaceID    agent.WorkspaceID `json:"workspace_id"`
	CreationCutoff uint64            `json:"creation_cutoff"`
	UpdatedAt      time.Time         `json:"updated_at"`
	SessionID      agent.SessionID   `json:"session_id"`
}

func (index *sessionListIndex) ensure() {
	if index.epoch == "" || index.created == nil {
		index.reset()
	}
}

func (index *sessionListIndex) reset() {
	var key [sha256.Size]byte
	if _, err := rand.Read(key[:]); err != nil {
		counter := sessionListFallbackIdentity.Add(1)
		binary.LittleEndian.PutUint64(key[:8], counter)
		binary.LittleEndian.PutUint64(key[8:16], uint64(time.Now().UnixNano()))
		key = sha256.Sum256(key[:16])
	}
	epoch := sha256.Sum256(append([]byte("agentslot.session-list.epoch/v1:"), key[:]...))
	index.epoch = base64.RawURLEncoding.EncodeToString(epoch[:16])
	index.cursorKey = key
	index.generation = 0
	index.created = make(map[agent.SessionID]uint64)
	index.lastUpdatedAt = time.Time{}
}

func (index *sessionListIndex) recordCreate(id agent.SessionID) time.Time {
	index.ensure()
	index.generation++
	index.created[id] = index.generation
	return index.nextUpdatedAt(time.Time{})
}

func (index *sessionListIndex) generationFor(id agent.SessionID) uint64 {
	index.ensure()
	return index.created[id]
}

func (index *sessionListIndex) observeUpdatedAt(value time.Time) {
	index.ensure()
	if value.After(index.lastUpdatedAt) {
		index.lastUpdatedAt = value
	}
}

func (index *sessionListIndex) nextUpdatedAt(previous time.Time) time.Time {
	index.ensure()
	next := time.Now().UTC()
	if !next.After(previous) {
		next = previous.Add(time.Nanosecond)
	}
	if !next.After(index.lastUpdatedAt) {
		next = index.lastUpdatedAt.Add(time.Nanosecond)
	}
	index.lastUpdatedAt = next
	return next
}

func (index *sessionListIndex) paginate(request ListRequest, sessions []listedSession) (ListResult, error) {
	index.ensure()
	limit := request.Limit
	if limit == 0 {
		limit = DefaultSessionListLimit
	}
	cursor := sessionListCursor{
		Version: sessionListCursorVersion, Epoch: index.epoch,
		AgentID: request.AgentID, WorkspaceID: request.WorkspaceID,
		CreationCutoff: index.generation,
	}
	if request.Cursor != "" {
		decoded, err := index.decodeCursor(request.Cursor)
		if err != nil {
			return ListResult{}, err
		}
		if decoded.Epoch != index.epoch || decoded.AgentID != request.AgentID || decoded.WorkspaceID != request.WorkspaceID || decoded.CreationCutoff > index.generation {
			return ListResult{}, errors.New("cursor does not belong to this Store lifecycle and scope")
		}
		cursor = decoded
	}

	eligible := make([]SessionSummary, 0, len(sessions))
	for _, candidate := range sessions {
		if candidate.generation > cursor.CreationCutoff {
			continue
		}
		if !cursor.UpdatedAt.IsZero() && !afterSessionListPosition(candidate.summary, cursor) {
			continue
		}
		eligible = append(eligible, candidate.summary)
	}
	sortSessionSummaries(eligible)
	if len(eligible) <= limit {
		return ListResult{Sessions: eligible}, nil
	}
	page := eligible[:limit:limit]
	last := page[len(page)-1]
	cursor.UpdatedAt = last.UpdatedAt
	cursor.SessionID = last.SessionID
	next, err := index.encodeCursor(cursor)
	if err != nil {
		return ListResult{}, err
	}
	return ListResult{Sessions: page, NextCursor: next}, nil
}

func afterSessionListPosition(summary SessionSummary, cursor sessionListCursor) bool {
	if summary.UpdatedAt.Before(cursor.UpdatedAt) {
		return true
	}
	return summary.UpdatedAt.Equal(cursor.UpdatedAt) && summary.SessionID > cursor.SessionID
}

func (index *sessionListIndex) encodeCursor(cursor sessionListCursor) (string, error) {
	payload, err := json.Marshal(cursor)
	if err != nil {
		return "", fmt.Errorf("encode cursor: %w", err)
	}
	signature := hmac.New(sha256.New, index.cursorKey[:])
	_, _ = signature.Write(payload)
	signed := append(payload, signature.Sum(nil)...)
	encoded := base64.RawURLEncoding.EncodeToString(signed)
	if len(encoded) > MaxSessionListCursorBytes {
		return "", errors.New("encoded cursor exceeds the portable size limit")
	}
	return encoded, nil
}

func (index *sessionListIndex) decodeCursor(encoded string) (sessionListCursor, error) {
	signed, err := base64.RawURLEncoding.DecodeString(encoded)
	if err != nil || len(signed) <= sha256.Size {
		return sessionListCursor{}, errors.New("cursor is malformed")
	}
	payload, provided := signed[:len(signed)-sha256.Size], signed[len(signed)-sha256.Size:]
	signature := hmac.New(sha256.New, index.cursorKey[:])
	_, _ = signature.Write(payload)
	if !hmac.Equal(provided, signature.Sum(nil)) {
		return sessionListCursor{}, errors.New("cursor signature is invalid")
	}
	decoder := json.NewDecoder(bytes.NewReader(payload))
	decoder.DisallowUnknownFields()
	var cursor sessionListCursor
	if err := decoder.Decode(&cursor); err != nil {
		return sessionListCursor{}, errors.New("cursor payload is malformed")
	}
	var trailing any
	if err := decoder.Decode(&trailing); err != io.EOF {
		return sessionListCursor{}, errors.New("cursor payload contains trailing data")
	}
	if cursor.Version != sessionListCursorVersion || cursor.Epoch == "" || !cursor.AgentID.Valid() || !cursor.WorkspaceID.Valid() || cursor.UpdatedAt.IsZero() || !cursor.SessionID.Valid() {
		return sessionListCursor{}, errors.New("cursor payload is invalid")
	}
	return cursor, nil
}

func sortSessionSummaries(summaries []SessionSummary) {
	sort.Slice(summaries, func(left, right int) bool {
		if !summaries[left].UpdatedAt.Equal(summaries[right].UpdatedAt) {
			return summaries[left].UpdatedAt.After(summaries[right].UpdatedAt)
		}
		return summaries[left].SessionID < summaries[right].SessionID
	})
}
