package main

import (
	"fmt"
	"testing"
	"time"
)

// TestSaveMessageToHistoryIdempotent verifies the fix for #292: persisting a
// message whose (user_id, message_id) already exists must NOT return an error
// (the plain INSERT previously violated the message_history unique constraint
// and was logged at ERROR on every HistorySync), and must not create a
// duplicate row.
func TestSaveMessageToHistoryIdempotent(t *testing.T) {
	s := makeTestServer(t)

	const (
		userID = "user-1"
		chat   = "123456@s.whatsapp.net"
		sender = "123456@s.whatsapp.net"
		msgID  = "MSG-DUP-1"
	)

	// First insert: a normal live Message event.
	if err := s.saveMessageToHistory(userID, chat, sender, msgID, "text", "hello", "", "", "{}"); err != nil {
		t.Fatalf("first insert failed: %v", err)
	}

	// Second insert with the same (user_id, message_id): simulates the same
	// message arriving again in a HistorySync batch or on reconnect. With the
	// fix this is a silent no-op; without it, it returns a unique-constraint
	// violation.
	if err := s.saveMessageToHistory(userID, chat, sender, msgID, "text", "hello", "", "", "{}"); err != nil {
		t.Fatalf("duplicate insert should be a silent no-op, got error: %v", err)
	}

	// Exactly one row must exist for this (user_id, message_id).
	var count int
	if err := s.db.Get(&count,
		"SELECT COUNT(*) FROM message_history WHERE user_id = ? AND message_id = ?",
		userID, msgID); err != nil {
		t.Fatalf("count query failed: %v", err)
	}
	if count != 1 {
		t.Fatalf("expected exactly 1 row after duplicate insert, got %d", count)
	}

	// A different message_id for the same user must still insert normally
	// (the conflict clause must not swallow legitimate inserts).
	if err := s.saveMessageToHistory(userID, chat, sender, "MSG-OTHER", "text", "world", "", "", "{}"); err != nil {
		t.Fatalf("insert of a distinct message failed: %v", err)
	}
	if err := s.db.Get(&count,
		"SELECT COUNT(*) FROM message_history WHERE user_id = ?", userID); err != nil {
		t.Fatalf("count query failed: %v", err)
	}
	if count != 2 {
		t.Fatalf("expected 2 distinct rows, got %d", count)
	}
}

// TestTrimMessageHistoryKeepsNewestWithinLimit pins the retention contract that
// the HistorySync batch path now depends on: trimMessageHistory must keep only
// the newest `limit` messages for a (user, chat) pair and leave other chats
// alone. HistorySync previously wrote its batches without ever trimming, which
// let message_history grow without bound.
func TestTrimMessageHistoryKeepsNewestWithinLimit(t *testing.T) {
	s := makeTestServer(t)

	// whatsmeow owns this table in production; trimMessageHistory clears the
	// matching secrets first, so the test schema needs it to exist.
	if _, err := s.db.Exec(`CREATE TABLE IF NOT EXISTS whatsmeow_message_secrets (
		message_id TEXT
	)`); err != nil {
		t.Fatalf("failed to create whatsmeow_message_secrets stub: %v", err)
	}

	const (
		userID    = "user-trim"
		chat      = "111@s.whatsapp.net"
		otherChat = "222@s.whatsapp.net"
		total     = 10
		keep      = 4
	)

	// saveMessageToHistory stamps time.Now(), which is not distinct enough
	// inside a loop to order by, so insert with explicit increasing timestamps.
	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	insert := func(chatJID, msgID string, ts time.Time) {
		t.Helper()
		if _, err := s.db.Exec(
			`INSERT INTO message_history (user_id, chat_jid, sender_jid, message_id, timestamp, message_type, text_content)
			 VALUES (?, ?, ?, ?, ?, ?, ?)`,
			userID, chatJID, "sender@s.whatsapp.net", msgID, ts, "text", msgID); err != nil {
			t.Fatalf("insert %s failed: %v", msgID, err)
		}
	}

	for i := 0; i < total; i++ {
		insert(chat, fmt.Sprintf("MSG-%02d", i), base.Add(time.Duration(i)*time.Minute))
	}
	// A second chat that must be left untouched.
	for i := 0; i < 3; i++ {
		insert(otherChat, fmt.Sprintf("OTHER-%02d", i), base.Add(time.Duration(i)*time.Minute))
	}

	if err := s.trimMessageHistory(userID, chat, keep); err != nil {
		t.Fatalf("trimMessageHistory failed: %v", err)
	}

	var remaining int
	if err := s.db.Get(&remaining,
		"SELECT COUNT(*) FROM message_history WHERE user_id = ? AND chat_jid = ?",
		userID, chat); err != nil {
		t.Fatalf("count query failed: %v", err)
	}
	if remaining != keep {
		t.Fatalf("expected %d rows to survive the trim, got %d", keep, remaining)
	}

	// The survivors must be the newest ones: MSG-06..MSG-09.
	var oldest string
	if err := s.db.Get(&oldest,
		`SELECT message_id FROM message_history
		 WHERE user_id = ? AND chat_jid = ? ORDER BY timestamp ASC LIMIT 1`,
		userID, chat); err != nil {
		t.Fatalf("oldest survivor query failed: %v", err)
	}
	if want := fmt.Sprintf("MSG-%02d", total-keep); oldest != want {
		t.Fatalf("expected oldest surviving message to be %s, got %s", want, oldest)
	}

	// Trimming one chat must not touch another.
	var otherRemaining int
	if err := s.db.Get(&otherRemaining,
		"SELECT COUNT(*) FROM message_history WHERE user_id = ? AND chat_jid = ?",
		userID, otherChat); err != nil {
		t.Fatalf("other-chat count query failed: %v", err)
	}
	if otherRemaining != 3 {
		t.Fatalf("expected the untrimmed chat to keep 3 rows, got %d", otherRemaining)
	}
}
