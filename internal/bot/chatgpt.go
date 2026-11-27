package bot

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/OMG-Software/omg-review-bot/internal/chatgpt"
	"github.com/OMG-Software/omg-review-bot/internal/settings"
)

// A ChatGPT plan model (settings.ProviderChatGPT) names a sign-in
// (settings.LLM.Account). The sign-in's tokens are stored apart from the
// models, because each renewal replaces them, sealed (see package secret)
// and bound to the set of models it was made for: the instance's, or one
// organization's own. One set's model can't name another set's sign-in and
// use it, as the seal only opens for the set it was made for.

// chatgptHostKey stores this installation's host ID, which OpenAI's sign-in
// takes to tell the bot's installations apart.
const chatgptHostKey = "chatgpt:host-id"

// chatgptKeys starts the keys of set's sign-ins, each followed by its ID:
// so a set's sign-ins can be found without its models.
func chatgptKeys(set int64) string { return "chatgpt:sign-in:" + strconv.FormatInt(set, 10) + ":" }

func chatgptKey(id string, set int64) string { return chatgptKeys(set) + id }

// errUnreadable: a sign-in that isn't set's (or whose seal doesn't open).
var errUnreadable = errors.New("the model's ChatGPT sign-in can't be read: sign in again on the Models page")

// unsavedSignIn is a renewed sign-in that couldn't be saved: kept, as it
// holds the only copy of the new refresh token, until it saves.
type unsavedSignIn struct {
	set  int64
	cred chatgpt.Credentials
}

// chatgptRevokeRetry are the waits before each new try at ending a
// sign-in at OpenAI, when the network or OpenAI failed.
var chatgptRevokeRetry = []time.Duration{5 * time.Second, 30 * time.Second, 2 * time.Minute}

func chatgptContext(id string, set int64) string {
	return "chatgpt:" + id + ":" + strconv.FormatInt(set, 10)
}

// chatgptRenewBefore: an access token this close to expiring is renewed
// before it's used.
const chatgptRenewBefore = 2 * time.Minute

// chatgptClient returns the client of OpenAI's sign-in (tests set
// b.chatgptAuth to their own).
func (b *Bot) chatgptClient() *chatgpt.Client {
	b.chatgptOnce.Do(func() {
		if b.chatgptAuth == nil {
			b.chatgptAuth = chatgpt.New()
		}
	})
	return b.chatgptAuth
}

// UseChatGPTAuth makes the bot sign ChatGPT accounts in through c rather
// than OpenAI (tests use a stand-in). It must come before the first use.
func (b *Bot) UseChatGPTAuth(c *chatgpt.Client) {
	b.chatgptOnce.Do(func() { b.chatgptAuth = c })
}

// chatgptLock is the lock a sign-in's renewal holds: its refresh token is
// replaced each time, so two renewals at once would end it.
func (b *Bot) chatgptLock(id string) *sync.Mutex {
	mu, _ := b.chatgptLocks.LoadOrStore(id, new(sync.Mutex))
	return mu.(*sync.Mutex)
}

// chatgptHostID returns this installation's host ID, made on first use and
// kept for good.
func (b *Bot) chatgptHostID() (string, error) {
	b.chatgptMu.Lock()
	defer b.chatgptMu.Unlock()
	if id, ok, err := b.store.Get(chatgptHostKey); err != nil || ok {
		return id, err
	}
	id, err := chatgpt.NewHostID()
	if err != nil {
		return "", err
	}
	return id, b.store.Set(chatgptHostKey, id)
}

// StartChatGPT starts signing a ChatGPT account in for a model of set: cur
// is the model as saved, whose earlier sign-in (if any) is signed in again.
// It returns the attempt, to keep until the callback comes, and the address
// that starts it in the browser.
func (b *Bot) StartChatGPT(set int64, cur settings.LLM) (chatgpt.Attempt, string, error) {
	host, err := b.chatgptHostID()
	if err != nil {
		return chatgpt.Attempt{}, "", fmt.Errorf("couldn't read the bot's host ID: %w", err)
	}
	a, err := chatgpt.NewAttempt(host, cur.ChatGPTClient, cur.AccountEmail)
	if err != nil {
		return chatgpt.Attempt{}, "", err
	}
	if cur.ChatGPTClient != "" {
		a.Subject = cur.AccountSubject // the account the client is for
		if cur.SignedIn() {
			a.IDTokenHint = b.chatgptIDToken(cur.Account, set)
		}
	}
	// A client kept without a sign-in is one whose plan use was declined
	// (see FinishChatGPT): asked for again, not just signed in.
	a.Consent = cur.ChatGPTClient != "" && !cur.SignedIn()
	return a, b.chatgptClient().AuthorizeURL(a), nil
}

// FinishChatGPT finishes an attempt with its callback (see
// chatgpt.ParseCallback) and stores the sign-in for set. It returns the
// sign-in's ID and who signed in (no tokens: those stay with the bot).
//
// A sign-in that didn't allow the plan's use returns chatgpt.ErrNoPlanUsage
// with who it was: the sign-in is ended (the bot can't use it), but its
// client is worth keeping, to ask for the plan's use again (StartChatGPT).
// One for another account than the model's (chatgpt.ErrOtherAccount) is
// ended too, and the model's own sign-in kept.
func (b *Bot) FinishChatGPT(ctx context.Context, set int64, a chatgpt.Attempt, cb chatgpt.Callback) (string, chatgpt.Credentials, error) {
	c := b.chatgptClient()
	cred, err := c.Finish(ctx, a, cb)
	if errors.Is(err, chatgpt.ErrNoPlanUsage) || errors.Is(err, chatgpt.ErrOtherAccount) {
		b.revokeChatGPT(set, cred)
		return "", withoutTokens(cred), err
	}
	if err != nil {
		return "", chatgpt.Credentials{}, err
	}
	id, err := newSignInID()
	if err != nil {
		return "", chatgpt.Credentials{}, err
	}
	if err := b.storeChatGPT(id, set, cred); err != nil {
		b.revokeChatGPT(set, cred)
		return "", chatgpt.Credentials{}, fmt.Errorf("couldn't save the ChatGPT sign-in: %w", err)
	}
	return id, withoutTokens(cred), nil
}

func withoutTokens(c chatgpt.Credentials) chatgpt.Credentials {
	c.IDToken, c.AccessToken, c.RefreshToken = "", "", ""
	return c
}

func newSignInID() (string, error) {
	id, err := chatgpt.NewHostID() // any random, unguessable ID
	if err != nil {
		return "", err
	}
	return id[len("urn:uuid:"):], nil
}

func (b *Bot) loadChatGPT(id string, set int64) (chatgpt.Credentials, error) {
	sealed, ok, err := b.store.Get(chatgptKey(id, set))
	if err != nil {
		return chatgpt.Credentials{}, fmt.Errorf("couldn't read the database: %w", err)
	}
	if !ok {
		return chatgpt.Credentials{}, errors.New("the model's ChatGPT sign-in is gone: sign in again on the Models page")
	}
	box, err := b.secretBox()
	if err != nil {
		return chatgpt.Credentials{}, err
	}
	raw, err := box.Open(sealed, chatgptContext(id, set))
	if err != nil {
		return chatgpt.Credentials{}, errUnreadable
	}
	var cred chatgpt.Credentials
	if err := json.Unmarshal(raw, &cred); err != nil {
		return chatgpt.Credentials{}, fmt.Errorf("the model's ChatGPT sign-in can't be read: %w", err)
	}
	return cred, nil
}

func (b *Bot) storeChatGPT(id string, set int64, cred chatgpt.Credentials) error {
	raw, err := json.Marshal(cred)
	if err != nil {
		return err
	}
	box, err := b.secretBox()
	if err != nil {
		return err
	}
	sealed, err := box.Seal(raw, chatgptContext(id, set))
	if err != nil {
		return err
	}
	return b.store.Set(chatgptKey(id, set), sealed)
}

// currentChatGPT returns a sign-in's latest credentials: a renewal that
// couldn't be saved (saving it now, if it can), or else the stored ones.
// The caller holds the sign-in's lock.
func (b *Bot) currentChatGPT(id string, set int64) (chatgpt.Credentials, error) {
	x, ok := b.chatgptUnsaved.Load(id)
	if !ok {
		return b.loadChatGPT(id, set)
	}
	u := x.(unsavedSignIn)
	if u.set != set {
		return chatgpt.Credentials{}, errUnreadable
	}
	if err := b.storeChatGPT(id, set, u.cred); err == nil {
		b.chatgptUnsaved.Delete(id)
	}
	return u.cred, nil
}

// chatgptIDToken is a sign-in's ID token, "" if it can't be read. It only
// names the account (an expired one does as well), so any copy will do and
// the sign-in's lock, held through a renewal, isn't waited for.
func (b *Bot) chatgptIDToken(id string, set int64) string {
	if x, ok := b.chatgptUnsaved.Load(id); ok {
		if u := x.(unsavedSignIn); u.set == set {
			return u.cred.IDToken
		}
		return ""
	}
	cred, err := b.loadChatGPT(id, set)
	if err != nil {
		return ""
	}
	return cred.IDToken
}

// DropChatGPT deletes a sign-in of set and ends it at OpenAI. An error says
// what's left undone, worded to follow "but" in a message about the model
// whose sign-in it was: the sign-in couldn't be read, or couldn't be
// deleted. It's ended at OpenAI all the same if it could be read.
func (b *Bot) DropChatGPT(id string, set int64) error {
	if id == "" {
		return nil
	}
	mu := b.chatgptLock(id)
	mu.Lock()
	defer mu.Unlock()
	_, ok, err := b.store.Get(chatgptKey(id, set))
	if err != nil {
		return fmt.Errorf("the bot couldn't read its ChatGPT sign-in to end it (%w): disconnect the app in ChatGPT's settings to be sure it's ended", err)
	}
	if !ok {
		return nil
	}
	cred, err := b.currentChatGPT(id, set)
	derr := b.store.Delete(chatgptKey(id, set))
	// Ended even if it couldn't be deleted: no model names it now, and a
	// copy left stored is of no use once OpenAI has ended it.
	b.chatgptUnsaved.Delete(id)
	b.chatgptLocks.Delete(id)
	if err != nil {
		// Its refresh token can't be read, so only the person can end it.
		b.chatgptUnconfirmed(set, "")
	} else {
		b.revokeChatGPT(set, cred)
	}
	switch {
	case derr == nil:
		return nil
	case err != nil:
		return fmt.Errorf("the bot couldn't delete or read its stored ChatGPT sign-in (%w): disconnect the app in ChatGPT's settings to be sure it's ended", derr)
	default:
		return fmt.Errorf("the bot couldn't delete its stored ChatGPT sign-in (%w), though OpenAI is still asked to end it", derr)
	}
}

// ChatGPTSignIns lists the IDs of every sign-in stored for set: the ones to
// drop when the set's models, which name them, can't be read.
func (b *Bot) ChatGPTSignIns(set int64) []string {
	keys, err := b.store.Keys(chatgptKeys(set))
	if err != nil {
		b.log.Warn("list ChatGPT sign-ins", "err", err)
		return nil
	}
	for i, k := range keys {
		keys[i] = strings.TrimPrefix(k, chatgptKeys(set))
	}
	return keys
}

// ForgottenChatGPT lists the sign-ins that before's models named and
// after's no longer do (a model removed, signed in again, or changed to
// another provider): the ones to drop once after is saved.
func ForgottenChatGPT(before, after settings.Models) []string {
	kept := map[string]bool{}
	for _, c := range after.Configs {
		kept[c.Account] = true
	}
	var gone []string
	for _, c := range before.Configs {
		if c.Account != "" && !kept[c.Account] {
			gone = append(gone, c.Account)
		}
	}
	return gone
}

// revokeChatGPT ends a sign-in at OpenAI, in the background (the bot has
// already let go of it), trying again while the network or OpenAI fails.
// If OpenAI never confirms it, set's activity says so (chatgptUnconfirmed).
func (b *Bot) revokeChatGPT(set int64, cred chatgpt.Credentials) {
	if cred.RefreshToken == "" {
		return // nothing left that renews
	}
	c := b.chatgptClient()
	waits := b.chatgptRetry
	if waits == nil {
		waits = chatgptRevokeRetry
	}
	go func() {
		for i := 0; ; i++ {
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			err := c.Revoke(ctx, cred)
			cancel()
			if err == nil {
				return
			}
			if !chatgpt.Temporary(err) || i == len(waits) {
				b.log.Warn("revoke ChatGPT sign-in", "err", err)
				b.chatgptUnconfirmed(set, cred.Email)
				return
			}
			time.Sleep(waits[i])
		}
	}()
}

// chatgptUnconfirmed tells set's activity that a sign-in the bot let go of
// may still be live at OpenAI, and how to end it.
func (b *Bot) chatgptUnconfirmed(set int64, email string) {
	who := "a ChatGPT account's sign-in"
	if email != "" {
		who = "the ChatGPT sign-in as " + email
	}
	b.store.LogAs(set, "warn", "", 0, fmt.Sprintf("OpenAI didn't confirm that %s was ended. To be sure it's ended, disconnect the app in ChatGPT's settings.", who))
}

// chatgptTokens gives a ChatGPT plan model its sign-in's access token
// (llm.TokenSource), renewing it as it expires.
type chatgptTokens struct {
	b   *Bot
	id  string
	set int64
}

func (t chatgptTokens) Token(ctx context.Context, refused string) (string, error) {
	// Under the sign-in's lock throughout: a renewal replaces the refresh
	// token, so only one runs at a time, and the calls waiting on it then
	// use its new token rather than renewing again.
	mu := t.b.chatgptLock(t.id)
	mu.Lock()
	defer mu.Unlock()
	cred, err := t.b.currentChatGPT(t.id, t.set)
	if err != nil {
		return "", err
	}
	if !cred.PlanUsage() {
		return "", chatgpt.ErrNoPlanUsage
	}
	if cred.AccessToken != refused && time.Until(cred.Expiry) > chatgptRenewBefore {
		return cred.AccessToken, nil
	}
	if cred.RefreshToken == "" {
		return "", chatgpt.ErrSignInEnded
	}
	// Not cut short with the job: once OpenAI has replaced the refresh
	// token, only its answer holds the new one, and the sign-in would be
	// lost without it.
	rctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
	defer cancel()
	if err := t.b.chatgptClient().Refresh(rctx, &cred); err != nil {
		return "", err
	}
	if cred.RefreshToken == "" {
		t.b.log.Warn("a ChatGPT renewal returned no new refresh token: the sign-in ends when its access token expires", "sign-in", t.id)
	}
	if err := t.b.storeChatGPT(t.id, t.set, cred); err != nil {
		// Kept until it saves (see currentChatGPT): the database holds the
		// refresh token OpenAI has just used up.
		t.b.chatgptUnsaved.Store(t.id, unsavedSignIn{set: t.set, cred: cred})
		t.b.log.Warn("save renewed ChatGPT sign-in (kept until it saves)", "err", err)
	} else {
		// An earlier renewal kept unsaved is now out of date: saved later,
		// it would put back the refresh token this one used up.
		t.b.chatgptUnsaved.Delete(t.id)
	}
	if !cred.PlanUsage() {
		return "", chatgpt.ErrNoPlanUsage
	}
	return cred.AccessToken, nil
}
