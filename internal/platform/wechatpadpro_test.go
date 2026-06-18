package platform

import "testing"

func TestParseInbound(t *testing.T) {
	const self = "wxid_self"
	cases := []struct {
		name    string
		raw     string
		wantOK  bool
		wantKey string
		wantTxt string
	}{
		{
			name:    "private text",
			raw:     `{"from_user_name":{"str":"wxid_peer"},"to_user_name":{"str":"wxid_self"},"content":{"str":"你好"},"msg_type":1}`,
			wantOK:  true,
			wantKey: "wxid_peer",
			wantTxt: "你好",
		},
		{
			name:   "own message ignored",
			raw:    `{"from_user_name":{"str":"wxid_self"},"content":{"str":"hi"},"msg_type":1}`,
			wantOK: false,
		},
		{
			name:   "official account ignored",
			raw:    `{"from_user_name":{"str":"gh_abc123"},"content":{"str":"hi"},"msg_type":1}`,
			wantOK: false,
		},
		{
			name:   "non-text ignored",
			raw:    `{"from_user_name":{"str":"wxid_peer"},"content":{"str":"<xml/>"},"msg_type":3}`,
			wantOK: false,
		},
		{
			name:   "group without @ ignored",
			raw:    `{"from_user_name":{"str":"12345@chatroom"},"content":{"str":"wxid_x:\n大家好"},"msg_type":1,"push_content":"某人 : 大家好"}`,
			wantOK: false,
		},
		{
			name:    "group @bot: prefix + mention stripped",
			raw:     `{"from_user_name":{"str":"12345@chatroom"},"content":{"str":"wxid_x:\n@jelly 在吗"},"msg_type":1,"push_content":"某人在群聊中@了你"}`,
			wantOK:  true,
			wantKey: "12345@chatroom",
			wantTxt: "在吗",
		},
		{
			name:   "empty text ignored",
			raw:    `{"from_user_name":{"str":"wxid_peer"},"content":{"str":"   "},"msg_type":1}`,
			wantOK: false,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			key, txt, ok := parseInbound([]byte(c.raw), self)
			if ok != c.wantOK {
				t.Fatalf("ok = %v, want %v (key=%q txt=%q)", ok, c.wantOK, key, txt)
			}
			if ok && (key != c.wantKey || txt != c.wantTxt) {
				t.Fatalf("got (%q,%q), want (%q,%q)", key, txt, c.wantKey, c.wantTxt)
			}
		})
	}
}
