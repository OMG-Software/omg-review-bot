package bot

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
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

func chatgptKey(id string) string { return "chatgpt:sign-in:" + id }

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

// StartChatGPT starts signing a ChatGPT account in for a model: cur is the
// model as saved, whose earlier sign-in (if any) is signed in again. It
// returns the attempt, to keep until the callback comes, and the address
// that starts it in the browser.
func (b *Bot) StartChatGPT(cur settings.LLM) (chatgpt.Attempt, string, error) {
	host, err := b.chatgptHostID()
	if err != nil {
		return chatgpt.Attempt{}, "", fmt.Errorf("couldn't read the bot's host ID: %w", err)
	}
	a, err := chatgpt.NewAttempt(host, cur.ChatGPTClient, cur.AccountEmail)
	if err != nil {
		return chatgpt.Attempt{}, "", err
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
func (b *Bot) FinishChatGPT(ctx context.Context, set int64, a chatgpt.Attempt, cb chatgpt.Callback) (string, chatgpt.Credentials, error) {
	c := b.chatgptClient()
	cred, err := c.Finish(ctx, a, cb)
	if errors.Is(err, chatgpt.ErrNoPlanUsage) {
		b.revokeChatGPT(cred)
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
		b.revokeChatGPT(cred)
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
	sealed, ok, err := b.store.Get(chatgptKey(id))
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
		return chatgpt.Credentials{}, errors.New("the model's ChatGPT sign-in can't be read: sign in again on the Models page")
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
	return b.store.Set(chatgptKey(id), sealed)
}

// DropChatGPT deletes a sign-in of set and ends it at OpenAI.
func (b *Bot) DropChatGPT(id string, set int64) {
	if id == "" {
		return
	}
	mu := b.chatgptLock(id)
	mu.Lock()
	defer mu.Unlock()
	cred, err := b.loadChatGPT(id, set)
	if err := b.store.Delete(chatgptKey(id)); err != nil {
		b.log.Warn("delete ChatGPT sign-in", "err", err)
		return
	}
	b.chatgptLocks.Delete(id)
	if err == nil {
		b.revokeChatGPT(cred)
	}
}

// ForgetChatGPT drops the sign-ins of set that before's models named and
// after's no longer do: a model removed, signed in again, or changed to
// another provider.
func (b *Bot) ForgetChatGPT(set int64, before, after settings.Models) {
	kept := map[string]bool{}
	for _, c := range after.Configs {
		kept[c.Account] = true
	}
	for _, c := range before.Configs {
		if c.Account != "" && !kept[c.Account] {
			b.DropChatGPT(c.Account, set)
		}
	}
}

// revokeChatGPT ends a sign-in at OpenAI, in the background: it's only
// tidying, as the bot has already let go of the tokens.
func (b *Bot) revokeChatGPT(cred chatgpt.Credentials) {
	c := b.chatgptClient()
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		if err := c.Revoke(ctx, cred); err != nil {
			b.log.Warn("revoke ChatGPT sign-in", "err", err)
		}
	}()
}

// chatgptTokens gives a ChatGPT plan model its sign-in's access token
// (llm.TokenSource), renewing it as it expires.
type chatgptTokens struct {
	b   *Bot
	id  string
	set int64
}

func (t chatgptTokens) Token(ctx context.Context, refused string) (string, error) {
	cred, err := t.b.loadChatGPT(t.id, t.set)
	if err != nil {
		return "", err
	}
	usable := func() bool { return cred.AccessToken != refused && time.Until(cred.Expiry) > chatgptRenewBefore }
	if usable() {
		return cred.AccessToken, nil
	}
	mu := t.b.chatgptLock(t.id)
	mu.Lock()
	defer mu.Unlock()
	// Another call may have renewed it while this one waited: its new token
	// is used rather than renewing again (which would replace it at once).
	if cred, err = t.b.loadChatGPT(t.id, t.set); err != nil {
		return "", err
	}
	if usable() {
		return cred.AccessToken, nil
	}
	// Not cut short with the job: once OpenAI has replaced the refresh
	// token, only its answer holds the new one, and the sign-in would be
	// lost without it.
	rctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
	defer cancel()
	if err := t.b.chatgptClient().Refresh(rctx, &cred); err != nil {
		return "", err
	}
	if err := t.b.storeChatGPT(t.id, t.set, cred); err != nil {
		return "", fmt.Errorf("couldn't save the renewed ChatGPT sign-in: %w", err)
	}
	if !cred.PlanUsage() {
		return "", chatgpt.ErrNoPlanUsage
	}
	return cred.AccessToken, nil
}
