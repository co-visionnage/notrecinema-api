//go:build integration

package families_test

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"notrecinema/api/internal/activitylog"
	"notrecinema/api/internal/families"
	"notrecinema/api/internal/platform/apperror"
	"notrecinema/api/internal/postgres"
)

func uniqueHex(t *testing.T) string {
	t.Helper()
	buf := make([]byte, 8)
	if _, err := rand.Read(buf); err != nil {
		t.Fatalf("rand: %v", err)
	}
	return hex.EncodeToString(buf)
}

// userWithEmail создаёт подтверждённого пользователя и возвращает id и email.
func userWithEmail(t *testing.T, ctx context.Context, db *postgres.Pool) (id, email string) {
	t.Helper()
	token := uniqueHex(t)
	hash := sha256.Sum256([]byte(token))
	email = fmt.Sprintf("invite-test-%s@example.com", token)

	if err := db.QueryRow(ctx,
		"SELECT user_id FROM public.create_profile_session($1, $2, $3, $4)",
		email, "Invite Test", hex.EncodeToString(hash[:]), time.Now().Add(time.Hour),
	).Scan(&id); err != nil {
		t.Fatalf("create user: %v", err)
	}
	return id, email
}

func hashOf(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}

// issueToken делает то же, что воркер при отправке письма.
func issueToken(t *testing.T, db *postgres.Pool, invitationID string, ttl time.Duration) string {
	t.Helper()
	token := "inv-" + uniqueHex(t)
	var ok bool
	if err := db.QueryRow(context.Background(),
		`SELECT public.create_family_invitation_token($1, $2, $3)`,
		invitationID, hashOf(token), time.Now().Add(ttl),
	).Scan(&ok); err != nil || !ok {
		t.Fatalf("create_family_invitation_token: ok=%v err=%v", ok, err)
	}
	return token
}

func eventCount(t *testing.T, db *postgres.Pool, aggregateID, eventType string) int {
	t.Helper()
	var n int
	if err := db.QueryRow(context.Background(), `
		SELECT count(*) FROM public.outbox_events WHERE aggregate_id = $1 AND event_type = $2
	`, aggregateID, eventType).Scan(&n); err != nil {
		t.Fatalf("count events: %v", err)
	}
	return n
}

func isKind(err error, kind apperror.Kind) bool {
	var appErr *apperror.Error
	return errors.As(err, &appErr) && appErr.Kind == kind
}

func setupFamily(t *testing.T) (db *postgres.Pool, svc *families.Service, ownerID string, family families.Family) {
	t.Helper()
	db = connectOrSkip(t)
	svc = families.NewService(db)
	ctx := context.Background()
	ownerID, _ = userWithEmail(t, ctx, db)
	family, err := svc.Create(ctx, ownerID, "Invite Family")
	if err != nil {
		t.Fatalf("Create() error: %v", err)
	}
	return db, svc, ownerID, family
}

func TestCreateInvitationQueuesEventAndListsIt(t *testing.T) {
	db, svc, ownerID, family := setupFamily(t)
	ctx := context.Background()
	email := fmt.Sprintf("Friend-%s@Example.com", uniqueHex(t))

	invitation, err := svc.CreateInvitation(ctx, ownerID, family.ID, "  "+email+"  ")
	if err != nil {
		t.Fatalf("CreateInvitation() error: %v", err)
	}
	if want := strings.ToLower(email); invitation.Email != want {
		t.Errorf("email = %q, want normalized %q", invitation.Email, want)
	}

	var payloadInvitationID string
	if err := db.QueryRow(ctx, `
		SELECT payload->>'invitationId' FROM public.outbox_events
		WHERE aggregate_id = $1 AND event_type = 'family.invitation_requested'
	`, family.ID).Scan(&payloadInvitationID); err != nil {
		t.Fatalf("event not found: %v", err)
	}
	if payloadInvitationID != invitation.ID {
		t.Errorf("event invitationId = %q, want %q", payloadInvitationID, invitation.ID)
	}

	list, err := svc.ListInvitations(ctx, ownerID, family.ID)
	if err != nil || len(list) != 1 || list[0].ID != invitation.ID {
		t.Errorf("ListInvitations() = (%+v, %v)", list, err)
	}
}

func TestInvitationsAreManagerOnly(t *testing.T) {
	db, svc, ownerID, family := setupFamily(t)
	ctx := context.Background()

	memberID, _ := userWithEmail(t, ctx, db)
	if _, err := svc.Join(ctx, memberID, family.InviteCode); err != nil {
		t.Fatalf("Join() error: %v", err)
	}
	strangerID, _ := userWithEmail(t, ctx, db)

	target := fmt.Sprintf("target-%s@example.com", uniqueHex(t))
	if _, err := svc.CreateInvitation(ctx, memberID, family.ID, target); !isKind(err, apperror.KindForbidden) {
		t.Errorf("a plain member could invite: err = %v", err)
	}
	if _, err := svc.CreateInvitation(ctx, strangerID, family.ID, target); !isKind(err, apperror.KindForbidden) {
		t.Errorf("a stranger could invite: err = %v", err)
	}

	inv, err := svc.CreateInvitation(ctx, ownerID, family.ID, target)
	if err != nil {
		t.Fatalf("owner CreateInvitation() error: %v", err)
	}

	if list, _ := svc.ListInvitations(ctx, memberID, family.ID); len(list) != 0 {
		t.Errorf("a plain member can see invitations: %+v", list)
	}
	if list, _ := svc.ListInvitations(ctx, strangerID, family.ID); len(list) != 0 {
		t.Errorf("a stranger can see invitations: %+v", list)
	}
	if err := svc.RevokeInvitation(ctx, memberID, family.ID, inv.ID); !isKind(err, apperror.KindNotFound) {
		t.Errorf("a plain member could revoke: err = %v", err)
	}

	// Админ -- менеджер: может и приглашать, и видеть.
	if err := svc.SetMemberRole(ctx, ownerID, family.ID, memberID, "admin"); err != nil {
		t.Fatalf("SetMemberRole() error: %v", err)
	}
	if _, err := svc.CreateInvitation(ctx, memberID, family.ID, fmt.Sprintf("by-admin-%s@example.com", uniqueHex(t))); err != nil {
		t.Errorf("an admin could not invite: %v", err)
	}
	if list, _ := svc.ListInvitations(ctx, memberID, family.ID); len(list) != 2 {
		t.Errorf("an admin sees %d invitations, want 2", len(list))
	}
}

func TestCreateInvitationValidation(t *testing.T) {
	_, svc, ownerID, family := setupFamily(t)
	ctx := context.Background()

	for _, bad := range []string{"", "   ", "not-an-email", "a@b", "two words@example.com"} {
		if _, err := svc.CreateInvitation(ctx, ownerID, family.ID, bad); !isKind(err, apperror.KindInvalidInput) {
			t.Errorf("CreateInvitation(%q) err = %v, want invalid input", bad, err)
		}
	}
}

func TestCreateInvitationRejectsExistingMemberAndReusesPendingRow(t *testing.T) {
	db, svc, ownerID, family := setupFamily(t)
	ctx := context.Background()

	memberID, memberEmail := userWithEmail(t, ctx, db)
	if _, err := svc.Join(ctx, memberID, family.InviteCode); err != nil {
		t.Fatalf("Join() error: %v", err)
	}
	if _, err := svc.CreateInvitation(ctx, ownerID, family.ID, memberEmail); !isKind(err, apperror.KindConflict) {
		t.Errorf("inviting an existing member: err = %v, want conflict", err)
	}

	target := fmt.Sprintf("again-%s@example.com", uniqueHex(t))
	first, err := svc.CreateInvitation(ctx, ownerID, family.ID, target)
	if err != nil {
		t.Fatalf("first CreateInvitation() error: %v", err)
	}
	second, err := svc.CreateInvitation(ctx, ownerID, family.ID, "  "+target+"  ")
	if err != nil {
		t.Fatalf("second CreateInvitation() error: %v", err)
	}
	if first.ID != second.ID {
		t.Errorf("a repeated invitation created a new row: %q vs %q", first.ID, second.ID)
	}
	if got := eventCount(t, db, family.ID, "family.invitation_requested"); got != 2 {
		t.Errorf("invitation events = %d, want 2 (the letter is sent again)", got)
	}
	if list, _ := svc.ListInvitations(ctx, ownerID, family.ID); len(list) != 1 {
		t.Errorf("pending invitations = %d, want 1", len(list))
	}
}

func TestPendingInvitationsPerFamilyAreCapped(t *testing.T) {
	db, svc, ownerID, family := setupFamily(t)
	ctx := context.Background()

	adminID, _ := userWithEmail(t, ctx, db)
	if _, err := svc.Join(ctx, adminID, family.InviteCode); err != nil {
		t.Fatalf("Join() error: %v", err)
	}
	if err := svc.SetMemberRole(ctx, ownerID, family.ID, adminID, "admin"); err != nil {
		t.Fatalf("SetMemberRole() error: %v", err)
	}

	// 12 от владельца и 8 от админа = 20 неотвеченных.
	for i := 0; i < 12; i++ {
		if _, err := svc.CreateInvitation(ctx, ownerID, family.ID, fmt.Sprintf("cap-o%d-%s@example.com", i, uniqueHex(t))); err != nil {
			t.Fatalf("owner invite %d: %v", i, err)
		}
	}
	for i := 0; i < 8; i++ {
		if _, err := svc.CreateInvitation(ctx, adminID, family.ID, fmt.Sprintf("cap-a%d-%s@example.com", i, uniqueHex(t))); err != nil {
			t.Fatalf("admin invite %d: %v", i, err)
		}
	}
	if _, err := svc.CreateInvitation(ctx, ownerID, family.ID, fmt.Sprintf("cap-over-%s@example.com", uniqueHex(t))); !isKind(err, apperror.KindConflict) {
		t.Errorf("the 21st pending invitation: err = %v, want conflict", err)
	}
}

func TestInvitationsPerUserAreRateLimited(t *testing.T) {
	db, svc, ownerID, family := setupFamily(t)
	ctx := context.Background()
	_ = db

	var limited bool
	for i := 0; i < 25; i++ {
		_, err := svc.CreateInvitation(ctx, ownerID, family.ID, fmt.Sprintf("rate-%d-%s@example.com", i, uniqueHex(t)))
		if isKind(err, apperror.KindRateLimited) {
			limited = true
			if i < 20 {
				t.Errorf("rate limit hit too early, at invitation %d", i+1)
			}
			break
		}
		if err != nil && !isKind(err, apperror.KindConflict) {
			t.Fatalf("invite %d: %v", i, err)
		}
	}
	if !limited {
		t.Error("no rate limit after 25 invitations in a day")
	}
}

func TestAcceptInvitationJoinsOnceAndFiresMemberJoined(t *testing.T) {
	db, svc, ownerID, family := setupFamily(t)
	ctx := context.Background()
	guestID, _ := userWithEmail(t, ctx, db)

	inv, err := svc.CreateInvitation(ctx, ownerID, family.ID, fmt.Sprintf("guest-%s@example.com", uniqueHex(t)))
	if err != nil {
		t.Fatalf("CreateInvitation() error: %v", err)
	}
	token := issueToken(t, db, inv.ID, 7*24*time.Hour)
	// Создание семьи уже пишет событие о владельце: считаем только новое.
	joinedBefore := eventCount(t, db, family.ID, "family.member.joined")

	joined, err := svc.AcceptInvitation(ctx, guestID, token)
	if err != nil {
		t.Fatalf("AcceptInvitation() error: %v", err)
	}
	if joined.ID != family.ID || joined.Name != "Invite Family" || joined.InviteCode == "" {
		t.Errorf("AcceptInvitation() = %+v", joined)
	}

	members, err := svc.ListMembers(ctx, ownerID, family.ID)
	if err != nil || len(members) != 2 {
		t.Fatalf("members after accepting = (%d, %v), want 2", len(members), err)
	}
	if got := eventCount(t, db, family.ID, "family.member.joined") - joinedBefore; got != 1 {
		t.Errorf("new member.joined events = %d, want 1", got)
	}

	// Вступление по приглашению попадает в журнал активности.
	activity, err := activitylog.NewService(db).ListForFamily(ctx, ownerID, family.ID, 0)
	if err != nil {
		t.Fatalf("activity log error: %v", err)
	}
	var joinEntries int
	for _, entry := range activity.Entries {
		if entry.Action == "member_joined" {
			joinEntries++
		}
	}
	if joinEntries != 1 {
		t.Errorf("member_joined activity entries = %d, want 1", joinEntries)
	}

	if _, err := svc.AcceptInvitation(ctx, guestID, token); err == nil {
		t.Error("the same invitation token worked twice")
	}
	if list, _ := svc.ListInvitations(ctx, ownerID, family.ID); len(list) != 0 {
		t.Errorf("an accepted invitation is still pending: %+v", list)
	}
}

func TestAcceptInvitationRejectsBadTokens(t *testing.T) {
	db, svc, ownerID, family := setupFamily(t)
	ctx := context.Background()
	guestID, _ := userWithEmail(t, ctx, db)

	if _, err := svc.AcceptInvitation(ctx, guestID, "nope"); err == nil {
		t.Error("an unknown token was accepted")
	}
	if _, err := svc.AcceptInvitation(ctx, guestID, "   "); !isKind(err, apperror.KindInvalidInput) {
		t.Errorf("a blank token: err = %v, want invalid input", err)
	}

	expired, err := svc.CreateInvitation(ctx, ownerID, family.ID, fmt.Sprintf("exp-%s@example.com", uniqueHex(t)))
	if err != nil {
		t.Fatalf("CreateInvitation() error: %v", err)
	}
	if _, err := svc.AcceptInvitation(ctx, guestID, issueToken(t, db, expired.ID, -time.Minute)); err == nil {
		t.Error("an expired token was accepted")
	}

	revoked, err := svc.CreateInvitation(ctx, ownerID, family.ID, fmt.Sprintf("rev-%s@example.com", uniqueHex(t)))
	if err != nil {
		t.Fatalf("CreateInvitation() error: %v", err)
	}
	token := issueToken(t, db, revoked.ID, time.Hour)
	if err := svc.RevokeInvitation(ctx, ownerID, family.ID, revoked.ID); err != nil {
		t.Fatalf("RevokeInvitation() error: %v", err)
	}
	if _, err := svc.AcceptInvitation(ctx, guestID, token); err == nil {
		t.Error("a revoked invitation was accepted")
	}

	if members, _ := svc.ListMembers(ctx, ownerID, family.ID); len(members) != 1 {
		t.Errorf("rejected tokens added members: %d", len(members))
	}
}

func TestAcceptInvitationByExistingMemberDoesNotDuplicate(t *testing.T) {
	db, svc, ownerID, family := setupFamily(t)
	ctx := context.Background()

	inv, err := svc.CreateInvitation(ctx, ownerID, family.ID, fmt.Sprintf("self-%s@example.com", uniqueHex(t)))
	if err != nil {
		t.Fatalf("CreateInvitation() error: %v", err)
	}
	token := issueToken(t, db, inv.ID, time.Hour)
	joinedBefore := eventCount(t, db, family.ID, "family.member.joined")

	joined, err := svc.AcceptInvitation(ctx, ownerID, token)
	if err != nil {
		t.Fatalf("AcceptInvitation() by an existing member error: %v", err)
	}
	if joined.ID != family.ID {
		t.Errorf("family = %+v", joined)
	}
	if got := eventCount(t, db, family.ID, "family.member.joined") - joinedBefore; got != 0 {
		t.Errorf("new member.joined events = %d, want 0: nothing changed", got)
	}
}

func TestPreviewInvitation(t *testing.T) {
	db, svc, ownerID, family := setupFamily(t)
	ctx := context.Background()

	inv, err := svc.CreateInvitation(ctx, ownerID, family.ID, fmt.Sprintf("prev-%s@example.com", uniqueHex(t)))
	if err != nil {
		t.Fatalf("CreateInvitation() error: %v", err)
	}

	if _, err := svc.PreviewInvitation(ctx, "no-token-yet"); !isKind(err, apperror.KindNotFound) {
		t.Errorf("preview of an unknown token: err = %v, want not found", err)
	}

	token := issueToken(t, db, inv.ID, time.Hour)
	preview, err := svc.PreviewInvitation(ctx, token)
	if err != nil {
		t.Fatalf("PreviewInvitation() error: %v", err)
	}
	if preview.FamilyName != "Invite Family" || preview.InviterName == "" {
		t.Errorf("preview = %+v", preview)
	}

	expired := issueToken(t, db, inv.ID, -time.Minute)
	if _, err := svc.PreviewInvitation(ctx, expired); !isKind(err, apperror.KindNotFound) {
		t.Errorf("preview of an expired token: err = %v, want not found", err)
	}
}

func TestResendInvitationIsLimitedAndScoped(t *testing.T) {
	db, svc, ownerID, family := setupFamily(t)
	ctx := context.Background()
	strangerID, _ := userWithEmail(t, ctx, db)

	inv, err := svc.CreateInvitation(ctx, ownerID, family.ID, fmt.Sprintf("resend-%s@example.com", uniqueHex(t)))
	if err != nil {
		t.Fatalf("CreateInvitation() error: %v", err)
	}
	if err := svc.ResendInvitation(ctx, strangerID, family.ID, inv.ID); !isKind(err, apperror.KindNotFound) {
		t.Errorf("a stranger could resend: err = %v", err)
	}

	for i := 0; i < 3; i++ {
		if err := svc.ResendInvitation(ctx, ownerID, family.ID, inv.ID); err != nil {
			t.Fatalf("resend %d: %v", i+1, err)
		}
	}
	// 1 при создании + 3 повторных.
	if got := eventCount(t, db, family.ID, "family.invitation_requested"); got != 4 {
		t.Errorf("invitation events = %d, want 4", got)
	}
	if err := svc.ResendInvitation(ctx, ownerID, family.ID, inv.ID); !isKind(err, apperror.KindRateLimited) {
		t.Errorf("the 4th resend within an hour: err = %v, want rate limited", err)
	}
}
