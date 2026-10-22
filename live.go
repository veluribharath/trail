package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"reflect"
	"strconv"
	"time"
)

// Live tailing. While an agent is still writing a session, the viewer keeps a
// Server-Sent Events stream open and the server sends what changed.
//
// Both agents only ever append whole JSON lines to the session file, and lock
// a separate file (if any) to keep two writers off one session, so reading it
// can't disturb the agent. The file is only stat'ed between reads and re-read
// whole with readComplete, which closes it straight away: on Windows a held
// handle would stop the agent from renaming or deleting the file.

var (
	livePoll = time.Second
	liveIdle = 10 * time.Minute // close the stream after this long with no new lines
)

// liveUpdate replaces the viewer's events from From on, and its changes from
// ChangesFrom on. An earlier event can change too: a tool call gets its output
// when the result line arrives.
type liveUpdate struct {
	From        int          `json:"from"`
	Events      []Event      `json:"events"`
	ChangesFrom int          `json:"changesFrom"`
	Changes     []FileChange `json:"changes"`
	Session     *Session     `json:"session"`
	Size        int64        `json:"size"`
}

// handleLive streams updates to a transcript the viewer loaded at ?size=.
// Events: "update" (a liveUpdate), "idle" (no new lines for a while) and
// "gone" (the file was moved or deleted). The viewer closes the stream on the
// last two, so the browser doesn't reconnect.
func (s *Server) handleLive(w http.ResponseWriter, r *http.Request) {
	key := r.URL.Query().Get("id")
	sess := s.index.Get(key)
	flusher, ok := w.(http.Flusher)
	if sess == nil || !ok {
		fail(w, http.StatusNotFound, "That session is no longer on disk. Refresh the list.")
		return
	}
	size, _ := strconv.ParseInt(r.URL.Query().Get("size"), 10, 64)

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	send := func(event string, v any) bool {
		b, _ := json.Marshal(v)
		if _, err := fmt.Fprintf(w, "event: %s\ndata: %s\n\n", event, b); err != nil {
			return false
		}
		flusher.Flush()
		return true
	}

	// The viewer's copy is the file's first `size` bytes. Re-read now: if the
	// file is still that size this is the viewer's transcript, and the diffs
	// start from it; otherwise send everything once.
	prev, err := s.index.Load(key)
	if err != nil {
		send("gone", nil)
		return
	}
	if prev.Size != size {
		if !send("update", diffTranscripts(&Transcript{}, prev)) {
			return
		}
	}
	if !send("ready", nil) {
		return
	}

	var lastSize int64 = -1
	var lastMod time.Time
	grew := time.Now()
	wait := livePoll
	for {
		select {
		case <-r.Context().Done():
			return
		case <-time.After(wait):
		}
		fi, err := os.Stat(sess.Path)
		if err != nil {
			send("gone", nil)
			return
		}
		if fi.Size() == lastSize && fi.ModTime().Equal(lastMod) {
			if time.Since(grew) > liveIdle {
				send("idle", nil)
				return
			}
			continue
		}
		lastSize, lastMod = fi.Size(), fi.ModTime()

		start := time.Now()
		t, err := s.index.Load(key)
		if err != nil {
			send("gone", nil)
			return
		}
		// Re-reading a big session costs real time; back off so a busy agent
		// can't keep trail parsing flat out.
		wait = max(livePoll, 4*time.Since(start))
		if t.Size == prev.Size {
			continue // only a partial line so far
		}
		grew = time.Now()
		if !send("update", diffTranscripts(prev, t)) {
			return
		}
		prev = t
	}
}

// diffTranscripts returns the update that turns a into b.
func diffTranscripts(a, b *Transcript) liveUpdate {
	from := firstDiff(len(a.Events), len(b.Events), func(i int) bool {
		return reflect.DeepEqual(a.Events[i], b.Events[i])
	})
	cfrom := firstDiff(len(a.Changes), len(b.Changes), func(i int) bool {
		return reflect.DeepEqual(a.Changes[i], b.Changes[i])
	})
	return liveUpdate{
		From:        from,
		Events:      b.Events[from:],
		ChangesFrom: cfrom,
		Changes:     b.Changes[cfrom:],
		Session:     b.Session,
		Size:        b.Size,
	}
}

// firstDiff returns the first index where two lists of lengths na and nb
// differ, given same(i) for indices both have.
func firstDiff(na, nb int, same func(int) bool) int {
	i := 0
	for i < na && i < nb && same(i) {
		i++
	}
	return i
}
