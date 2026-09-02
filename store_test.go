// GatewayStore 测试：读写往返、归一化校验、下游 key 生成。
package main

import (
	"os"
	"path/filepath"
	"testing"
)

func newTestStore(t *testing.T) *GatewayStore {
	t.Helper()
	s := newGatewayStore(filepath.Join(t.TempDir(), "gateway.json"))
	if err := s.load(); err != nil {
		t.Fatalf("load: %v", err)
	}
	return s
}

func TestStoreChannelRoundtrip(t *testing.T) {
	s := newTestStore(t)
	ch := &Channel{
		Name: "Cline", BaseURL: "https://api.cline.bot/api/v1",
		Rewrite: true, Enabled: true,
		Keys: []*UpKey{
			{Name: "acc1", APIKey: "sk-a", Enabled: true},
			{Name: "acc2", APIKey: "sk-b", Enabled: false, Proxy: &ProxySpec{Kind: "static", URL: "socks5://1.2.3.4:1080"}},
		},
	}
	if err := s.PutChannel(ch); err != nil {
		t.Fatalf("PutChannel: %v", err)
	}
	if ch.ID == "" || ch.Keys[0].ID == "" {
		t.Fatal("expected ids to be generated")
	}

	snap := s.Snapshot()
	if len(snap.Channels) != 1 || snap.Channels[0].Name != "Cline" {
		t.Fatalf("unexpected snapshot: %+v", snap.Channels)
	}
	if snap.Channels[0].Keys[1].Proxy.Kind != "static" {
		t.Fatalf("proxy spec lost: %+v", snap.Channels[0].Keys[1].Proxy)
	}
}

func TestStoreChannelValidation(t *testing.T) {
	s := newTestStore(t)
	if err := s.PutChannel(&Channel{Name: "", BaseURL: "http://x"}); err == nil {
		t.Fatal("expected name required")
	}
	if err := s.PutChannel(&Channel{Name: "x", BaseURL: ""}); err == nil {
		t.Fatal("expected base_url required")
	}
	if err := s.PutChannel(&Channel{Name: "x", BaseURL: "http://x", Keys: []*UpKey{{Name: "k", Enabled: true, Proxy: &ProxySpec{Kind: "static"}}}}); err == nil {
		t.Fatal("expected static proxy url required")
	}
	if err := s.PutChannel(&Channel{Name: "x", BaseURL: "http://x", Keys: []*UpKey{{Name: "k", Enabled: true, Proxy: &ProxySpec{Kind: "bogus"}}}}); err == nil {
		t.Fatal("expected bogus proxy kind rejected")
	}
	// ipv6pool 无 scheme 自动补 http://
	ch := &Channel{Name: "x", BaseURL: "http://x", Enabled: true, Keys: []*UpKey{{Name: "k", Enabled: true, Proxy: &ProxySpec{Kind: "ipv6pool", PoolURL: "1.2.3.4:8080"}}}}
	if err := s.PutChannel(ch); err != nil {
		t.Fatalf("ipv6pool normalize: %v", err)
	}
	if got := s.Snapshot().Channels[0].Keys[0].Proxy.PoolURL; got != "http://1.2.3.4:8080" {
		t.Fatalf("pool url normalized to %q", got)
	}
}

func TestStoreUpdateAndDelete(t *testing.T) {
	s := newTestStore(t)
	ch := &Channel{Name: "a", BaseURL: "http://a", Enabled: true, Keys: []*UpKey{{Name: "k1", Enabled: true}}}
	_ = s.PutChannel(ch)
	ch.Name = "a2"
	ch.Keys = append(ch.Keys, &UpKey{Name: "k2", Enabled: true})
	if err := s.PutChannel(ch); err != nil {
		t.Fatalf("update: %v", err)
	}
	snap := s.Snapshot()
	if len(snap.Channels) != 1 || snap.Channels[0].Name != "a2" || len(snap.Channels[0].Keys) != 2 {
		t.Fatalf("update failed: %+v", snap.Channels)
	}
	if _, ok := s.DeleteChannel(ch.ID); !ok {
		t.Fatal("delete failed")
	}
	if len(s.Snapshot().Channels) != 0 {
		t.Fatal("expected empty channels")
	}
}

func TestStoreGWKeys(t *testing.T) {
	s := newTestStore(t)
	k := &GWKey{Name: "sub2api", Key: newGWKeyValue(), Enabled: true}
	if err := s.PutGWKey(k); err != nil {
		t.Fatalf("PutGWKey: %v", err)
	}
	if len(k.Key) < 16 || k.Key[:6] != "sk-gw-" {
		t.Fatalf("unexpected key format %q", k.Key)
	}
	if err := s.PutGWKey(&GWKey{Name: "", Key: "x"}); err == nil {
		t.Fatal("expected name required")
	}
	if _, ok := s.DeleteGWKey(k.ID); !ok {
		t.Fatal("delete failed")
	}
}

func TestStoreFindUpKey2(t *testing.T) {
	s := newTestStore(t)
	ch := &Channel{Name: "c", BaseURL: "http://c", Enabled: true, Keys: []*UpKey{{Name: "k", Enabled: true}}}
	_ = s.PutChannel(ch)
	kid := s.Snapshot().Channels[0].Keys[0].ID
	gotCh, gotK, ok := s.FindUpKey2(ch.ID, kid)
	if !ok || gotCh.Name != "c" || gotK.Name != "k" {
		t.Fatalf("FindUpKey2 failed: %v %+v", ok, gotK)
	}
	if _, _, ok := s.FindUpKey2("nope", kid); ok {
		t.Fatal("expected miss")
	}
}

func TestStoreAtomicSave(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "sub", "gateway.json")
	s := newGatewayStore(path)
	if err := s.load(); err != nil {
		t.Fatalf("load (creates file): %v", err)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("config file not created: %v", err)
	}
	// 无残留 .tmp
	if _, err := os.Stat(path + ".tmp"); err == nil {
		t.Fatal("tmp file should be renamed away")
	}
}
