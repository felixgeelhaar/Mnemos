package main

import (
	"os"
	"path/filepath"
	"strings"
	"time"
)

// Recall-driven durability classification.
//
// The demotion added for session-local beliefs can only act on beliefs that
// have a verdict, and `classify-durability` fills those in as a BULK pass —
// which is the one thing that cannot reach what a reader actually sees. Recall
// ranks by relevance to a question, so no static ordering over the brain
// predicts which beliefs surface: ordering by trust was measured and failed
// (trust is blind to durability and clustered, with 9,935 beliefs at >= 0.79),
// and recency discriminates only weakly (52% against 38%).
//
// The beliefs a brief just displayed are, by definition, exactly the ones worth
// a verdict. So recall hands those ids to a detached worker. Coverage then
// grows along the path the reader actually walks instead of sweeping a brain
// that takes hours to cover, and it converges: once a topic's beliefs are
// judged, recalling it again spawns nothing.
//
// Everything here is best-effort and off the critical path. A brief must never
// wait on, or fail because of, a classification.

// recallClassifyInterval throttles the worker. Recall runs on every prompt and
// the model is often local, so without a floor a fast exchange would queue
// several classifications at once and contend for the same GPU.
const recallClassifyInterval = 5 * time.Minute

func recallClassifyStamp() string {
	return filepath.Join(captureStateDir(), "recall-classify.stamp")
}

// recallClassifyDue reports whether enough time has passed since the last
// spawn. An unusable stamp reads as due, so a bad path cannot silence the
// fill-in permanently — the same failure direction as the health snapshot.
func recallClassifyDue(now time.Time) bool {
	info, err := os.Stat(recallClassifyStamp())
	if err != nil || !info.Mode().IsRegular() {
		return true
	}
	return now.Sub(info.ModTime()) >= recallClassifyInterval
}

func markRecallClassify(now time.Time) {
	path := recallClassifyStamp()
	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		return
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return
	}
	_ = f.Close()
	_ = os.Chtimes(path, now, now)
}

// maybeClassifyRecalled spawns a detached `classify-durability --ids` for the
// beliefs a brief just showed that have no verdict yet. Returns whether it
// spawned, for tests.
//
// Hosted brains are skipped: the worker would classify whichever store this
// machine resolves, which is not the brain the session is reading from.
func maybeClassifyRecalled(ids []string, now time.Time) bool {
	if len(ids) == 0 || hostedConfigured() || !recallClassifyDue(now) {
		return false
	}
	if !spawnWorker([]string{"classify-durability", "--ids", strings.Join(ids, ",")}) {
		return false
	}
	// Stamped on spawn rather than on success: if the classification itself
	// hangs, stamping on success would respawn it on every prompt.
	markRecallClassify(now)
	return true
}

// unjudgedIDs returns the ids of recalled beliefs that carry no durability
// verdict, capped so one prompt can never queue an unbounded amount of work.
func unjudgedIDs(claims []recallClaim, max int) []string {
	var out []string
	for _, c := range claims {
		if c.ID == "" || c.Judged {
			continue
		}
		out = append(out, c.ID)
		if max > 0 && len(out) >= max {
			break
		}
	}
	return out
}

// maxRecallClassifyIDs bounds one spawn to a couple of model batches. A brief
// shows a handful of beliefs; anything beyond that is not what the reader saw.
const maxRecallClassifyIDs = 20
