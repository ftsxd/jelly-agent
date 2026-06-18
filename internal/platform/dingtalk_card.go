package platform

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"sync"
	"sync/atomic"
	"time"
)

// dingOpenAPI is the DingTalk OpenAPI base for the AI-card endpoints.
const dingOpenAPI = "https://api.dingtalk.com"

var cardSeq atomic.Uint64

// dingCardClient drives DingTalk's streaming AI-card APIs: it trades the bot's
// AppKey/AppSecret for an access token (cached until expiry), creates+delivers a
// card instance bound to a template, then pushes the answer into it
// incrementally. This is what gives DingTalk replies a live, streaming feel
// (the Stream SDK only sends one-shot text). Mirrors LangBot's dingtalk card flow.
type dingCardClient struct {
	clientID     string
	clientSecret string
	http         *http.Client

	mu    sync.Mutex
	token string
	exp   time.Time
}

func newDingCardClient(clientID, clientSecret string) *dingCardClient {
	return &dingCardClient{clientID: clientID, clientSecret: clientSecret, http: &http.Client{Timeout: 15 * time.Second}}
}

// accessToken returns a valid token, refreshing it when missing or near expiry.
func (c *dingCardClient) accessToken(ctx context.Context) (string, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.token != "" && time.Now().Before(c.exp) {
		return c.token, nil
	}
	var r struct {
		AccessToken string `json:"accessToken"`
		ExpireIn    int    `json:"expireIn"`
	}
	body := map[string]any{"appKey": c.clientID, "appSecret": c.clientSecret}
	if err := c.do(ctx, http.MethodPost, dingOpenAPI+"/v1.0/oauth2/accessToken", "", body, &r); err != nil {
		return "", err
	}
	if r.AccessToken == "" {
		return "", fmt.Errorf("空 accessToken")
	}
	ttl := r.ExpireIn
	if ttl < 120 {
		ttl = 7200
	}
	c.token = r.AccessToken
	c.exp = time.Now().Add(time.Duration(ttl-60) * time.Second)
	return c.token, nil
}

// createCard creates and delivers a streaming card instance and returns its
// outTrackId (used to push updates). convType is "1" for 1:1 and "2" for group.
func (c *dingCardClient) createCard(ctx context.Context, templateID, convType, convID, senderStaffID, query string) (string, error) {
	tok, err := c.accessToken(ctx)
	if err != nil {
		return "", err
	}
	trackID := fmt.Sprintf("jelly-%d-%d", time.Now().UnixNano(), cardSeq.Add(1))
	body := map[string]any{
		"cardTemplateId": templateID,
		"outTrackId":     trackID,
		"cardData": map[string]any{"cardParamMap": map[string]any{
			"content": "", "config": `{"autoLayout":true}`, "query": query,
		}},
		"callbackType":          "STREAM",
		"imGroupOpenSpaceModel": map[string]any{"supportForward": true},
		"imRobotOpenSpaceModel": map[string]any{"supportForward": true},
	}
	if convType == "2" { // group
		body["openSpaceId"] = "dtv1.card//IM_GROUP." + convID
		body["imGroupOpenDeliverModel"] = map[string]any{"robotCode": c.clientID}
	} else { // 1:1
		body["openSpaceId"] = "dtv1.card//IM_ROBOT." + senderStaffID
		body["imRobotOpenDeliverModel"] = map[string]any{"spaceType": "IM_ROBOT"}
	}
	if err := c.do(ctx, http.MethodPost, dingOpenAPI+"/v1.0/card/instances/createAndDeliver", tok, body, nil); err != nil {
		return "", err
	}
	return trackID, nil
}

// streamCard replaces the card's content with the full text so far; finalize
// marks the stream complete.
func (c *dingCardClient) streamCard(ctx context.Context, trackID, content string, finalize bool) error {
	tok, err := c.accessToken(ctx)
	if err != nil {
		return err
	}
	body := map[string]any{
		"outTrackId": trackID,
		"guid":       fmt.Sprintf("%d-%d", time.Now().UnixNano(), cardSeq.Add(1)),
		"key":        "content",
		"content":    content,
		"isFull":     true,
		"isFinalize": finalize,
		"isError":    false,
	}
	return c.do(ctx, http.MethodPut, dingOpenAPI+"/v1.0/card/streaming", tok, body, nil)
}

// do issues a JSON request; token (when set) goes in the DingTalk access-token
// header. out may be nil to discard the response body.
func (c *dingCardClient) do(ctx context.Context, method, url, token string, body, out any) error {
	var rdr io.Reader
	if body != nil {
		bs, err := json.Marshal(body)
		if err != nil {
			return err
		}
		rdr = bytes.NewReader(bs)
	}
	req, err := http.NewRequestWithContext(ctx, method, url, rdr)
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	if token != "" {
		req.Header.Set("x-acs-dingtalk-access-token", token)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 1024))
		return fmt.Errorf("HTTP %d: %s", resp.StatusCode, string(b))
	}
	if out == nil {
		_, _ = io.Copy(io.Discard, resp.Body)
		return nil
	}
	return json.NewDecoder(resp.Body).Decode(out)
}
