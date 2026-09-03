package portmap

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/huin/goupnp/dcps/internetgateway2"
)

// fakeIGDv6 是跑在 httptest 之上的最小 IGD，只服务 WANIPv6FirewallControl:1 的设备描述与
// AddPinhole/DeletePinhole/UpdatePinhole 三个动作，验证 upnp.go 与 goupnp 生成代码之间的往返。
type fakeIGDv6 struct {
	mu         sync.Mutex
	adds       []pinholeAdd
	dels       []uint16
	updates    []pinholeUpdate
	nextID     uint16
	failUpdate bool // 下一次 UpdatePinhole 回 704 NoSuchEntry（模拟 pinhole 被网关回收）
	failDelete bool // 下一次 DeletePinhole 回 704（验证「已不存在视为成功」）
}

type pinholeAdd struct {
	internalClient string
	internalPort   int
	protocol       int
	remoteHost     string
	remotePort     int
	lease          int
}

type pinholeUpdate struct {
	id    int
	lease int
}

const wanIP6FCNS = "urn:schemas-upnp-org:service:WANIPv6FirewallControl:1"

func (f *fakeIGDv6) handler() http.HandlerFunc {
	const deviceXML = `<?xml version="1.0"?>
<root xmlns="urn:schemas-upnp-org:device-1-0">
  <specVersion><major>1</major><minor>0</minor></specVersion>
  <device>
    <deviceType>urn:schemas-upnp-org:device:InternetGatewayDevice:2</deviceType>
    <friendlyName>fake igd v6</friendlyName>
    <manufacturer>hearth-test</manufacturer>
    <modelName>fake</modelName>
    <UDN>uuid:fake-igd-6</UDN>
    <serviceList>
      <service>
        <serviceType>urn:schemas-upnp-org:service:WANIPv6FirewallControl:1</serviceType>
        <serviceId>urn:upnp-org:serviceId:WANIPv6FwCtrl1</serviceId>
        <controlURL>/ctl6</controlURL>
        <eventSubURL>/evt6</eventSubURL>
        <SCPDURL>/scpd6.xml</SCPDURL>
      </service>
    </serviceList>
  </device>
</root>`

	return func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/desc.xml":
			w.Header().Set("Content-Type", "text/xml")
			_, _ = w.Write([]byte(deviceXML))
		case r.Method == http.MethodPost && r.URL.Path == "/ctl6":
			f.serveSOAP(w, r)
		default:
			http.NotFound(w, r)
		}
	}
}

func (f *fakeIGDv6) serveSOAP(w http.ResponseWriter, r *http.Request) {
	action := r.Header.Get("SOAPACTION")
	body, _ := io.ReadAll(r.Body)
	switch {
	case containsAction(action, "AddPinhole"):
		f.mu.Lock()
		f.nextID++
		id := f.nextID
		f.adds = append(f.adds, pinholeAdd{
			internalClient: extractStringTag(body, "InternalClient"),
			internalPort:   extractIntTag(body, "InternalPort"),
			protocol:       extractIntTag(body, "Protocol"),
			remoteHost:     extractStringTag(body, "RemoteHost"),
			remotePort:     extractIntTag(body, "RemotePort"),
			lease:          extractIntTag(body, "LeaseTime"),
		})
		f.mu.Unlock()
		writeSOAPResult(w, wanIP6FCNS, "AddPinhole",
			"<UniqueID>"+strconv.Itoa(int(id))+"</UniqueID>")
	case containsAction(action, "UpdatePinhole"):
		f.mu.Lock()
		fail := f.failUpdate
		f.failUpdate = false
		f.updates = append(f.updates, pinholeUpdate{
			id:    extractIntTag(body, "UniqueID"),
			lease: extractIntTag(body, "NewLeaseTime"),
		})
		f.mu.Unlock()
		if fail {
			writeSOAPFault(w, upnp6NoSuchEntry, "NoSuchEntry")
			return
		}
		writeSOAPResult(w, wanIP6FCNS, "UpdatePinhole", "")
	case containsAction(action, "DeletePinhole"):
		f.mu.Lock()
		fail := f.failDelete
		f.failDelete = false
		f.dels = append(f.dels, uint16(extractIntTag(body, "UniqueID")))
		f.mu.Unlock()
		if fail {
			writeSOAPFault(w, upnp6NoSuchEntry, "NoSuchEntry")
			return
		}
		writeSOAPResult(w, wanIP6FCNS, "DeletePinhole", "")
	default:
		http.Error(w, "unknown action: "+action, http.StatusNotImplemented)
	}
}

func containsAction(soapAction, name string) bool {
	return strings.Contains(soapAction, name)
}

func TestUPnP6PinholeRoundTrip(t *testing.T) {
	fake := &fakeIGDv6{}
	srv := httptest.NewServer(fake.handler())
	defer srv.Close()

	descURL, err := url.Parse(srv.URL + "/desc.xml")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	clients, err := internetgateway2.NewWANIPv6FirewallControl1ClientsByURLCtx(ctx, descURL)
	if err != nil {
		t.Fatalf("NewWANIPv6FirewallControl1ClientsByURLCtx: %v", err)
	}
	if len(clients) != 1 {
		t.Fatalf("got %d WANIPv6FirewallControl1 clients, want 1", len(clients))
	}
	c := newUPnP6Client(clients[0])

	// 首次 Open：AddPinhole，验证参数（InternalClient=GUA、端口、协议号、租期）。
	h, err := c.Open(ctx, "udp", 47700, testGUA, time.Hour)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if !h.hasID || h.id != 1 {
		t.Fatalf("首条 pinhole 应解析出 UniqueID=1，得到 %+v", h)
	}
	fake.mu.Lock()
	if len(fake.adds) != 1 {
		fake.mu.Unlock()
		t.Fatalf("AddPinhole 调用数 = %d", len(fake.adds))
	}
	add := fake.adds[0]
	fake.mu.Unlock()
	if add.internalClient != testGUA.String() || add.internalPort != 47700 || add.protocol != 17 {
		t.Fatalf("AddPinhole 参数不对: %+v", add)
	}
	if add.remoteHost != "" || add.remotePort != 0 {
		t.Fatalf("RemoteHost/RemotePort 应为任意源（空/0）: %+v", add)
	}
	if add.lease != 3600 {
		t.Fatalf("租期 = %d，应为 3600", add.lease)
	}

	// 续租：走 UpdatePinhole，不再新增 AddPinhole。
	if _, err := c.Open(ctx, "udp", 47700, testGUA, time.Hour); err != nil {
		t.Fatalf("续租: %v", err)
	}
	fake.mu.Lock()
	if len(fake.adds) != 1 || len(fake.updates) != 1 || fake.updates[0].id != 1 {
		fake.mu.Unlock()
		t.Fatalf("续租应走 UpdatePinhole(1)：adds=%+v updates=%+v", fake.adds, fake.updates)
	}
	fake.mu.Unlock()

	// UpdatePinhole 报 704 unknown id → 重新 AddPinhole，拿到新的 UniqueID。
	fake.mu.Lock()
	fake.failUpdate = true
	fake.mu.Unlock()
	h2, err := c.Open(ctx, "udp", 47700, testGUA, time.Hour)
	if err != nil {
		t.Fatalf("704 后应重新 AddPinhole: %v", err)
	}
	if h2.id != 2 {
		t.Fatalf("重加后 UniqueID = %d，应为 2", h2.id)
	}
	fake.mu.Lock()
	if len(fake.adds) != 2 {
		fake.mu.Unlock()
		t.Fatalf("704 后应再 AddPinhole 一次，adds=%+v", fake.adds)
	}
	fake.mu.Unlock()

	// Close：DeletePinhole(当前 id)。
	if err := c.Close(ctx, h2); err != nil {
		t.Fatalf("Close: %v", err)
	}
	fake.mu.Lock()
	if len(fake.dels) != 1 || fake.dels[0] != 2 {
		fake.mu.Unlock()
		t.Fatalf("DeletePinhole 调用 = %+v，应删 id=2", fake.dels)
	}
	fake.mu.Unlock()

	// 再删一条并让网关回 704：已不存在视为成功。
	h3, err := c.Open(ctx, "udp", 8080, testGUA, time.Hour)
	if err != nil {
		t.Fatalf("Open 第三条: %v", err)
	}
	fake.mu.Lock()
	fake.failDelete = true
	fake.mu.Unlock()
	if err := c.Close(ctx, h3); err != nil {
		t.Fatalf("Close 遇 704 应视为成功，得到 %v", err)
	}
}

func TestDiscoverUPnP6NoDevice(t *testing.T) {
	// 没有真实 IGD 时（回环网络里没有多播 IGD 应答），发现应在超时后返回 ErrUnsupported，
	// 不 panic、不返回其它错误——让 Mapper 安静退避。
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	_, err := discoverUPnP6(ctx)
	if err == nil {
		t.Skip("本机网络里存在真实的 WANIPv6FirewallControl IGD，跳过负路径断言")
	}
}
