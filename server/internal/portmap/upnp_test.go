package portmap

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/huin/goupnp"
	"github.com/huin/goupnp/dcps/internetgateway2"
	"github.com/huin/goupnp/soap"
)

// TestClassifyUPnPError 覆盖方案里错误码表要求的分类：606/718/729/725/501 各自
// 落到对应的哨兵错误，未知错误码原样包装但保留错误码文字。
func TestClassifyUPnPError(t *testing.T) {
	cases := []struct {
		name string
		code int
		want error
	}{
		{"not authorized", 606, ErrNotAuthorized},
		{"conflict in mapping entry", 718, ErrConflict},
		{"conflict with other mechanisms", 729, ErrConflict},
		{"permanent only", 725, ErrPermanentOnly},
		{"no resources", 501, ErrNoResources},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			fault := &soap.SOAPFaultError{}
			fault.Detail.UPnPError.Errorcode = c.code
			fault.Detail.UPnPError.ErrorDescription = c.name
			got := classifyUPnPError(fault)
			if !errors.Is(got, c.want) {
				t.Fatalf("classifyUPnPError(%d) = %v, want wrapping %v", c.code, got, c.want)
			}
		})
	}

	t.Run("unknown code preserved", func(t *testing.T) {
		fault := &soap.SOAPFaultError{}
		fault.Detail.UPnPError.Errorcode = 402
		fault.Detail.UPnPError.ErrorDescription = "InvalidArgs"
		got := classifyUPnPError(fault)
		if !strings.Contains(got.Error(), "402") || !strings.Contains(got.Error(), "InvalidArgs") {
			t.Fatalf("classifyUPnPError(402) = %v, want it to mention code and description", got)
		}
	})

	t.Run("non-fault error passes through", func(t *testing.T) {
		plain := errors.New("network unreachable")
		if got := classifyUPnPError(plain); got != plain {
			t.Fatalf("classifyUPnPError(plain) = %v, want unchanged %v", got, plain)
		}
	})
}

// fakeConn 是 upnpConn 的最小实现，只用来驱动 Map 里 external==0 的分支判断，
// 不涉及真实网络调用。
type fakeConn struct {
	sc *goupnp.ServiceClient
}

func (f *fakeConn) AddPortMappingCtx(ctx context.Context, remoteHost string, externalPort uint16, protocol string, internalPort uint16, internalClient string, enabled bool, description string, leaseDuration uint32) error {
	return fmt.Errorf("unexpected call to AddPortMapping in this test")
}

func (f *fakeConn) DeletePortMappingCtx(ctx context.Context, remoteHost string, externalPort uint16, protocol string) error {
	return fmt.Errorf("unexpected call to DeletePortMapping in this test")
}

func (f *fakeConn) GetExternalIPAddressCtx(ctx context.Context) (string, error) {
	return "", fmt.Errorf("unexpected call to GetExternalIPAddress in this test")
}

func (f *fakeConn) GetServiceClient() *goupnp.ServiceClient { return f.sc }

// TestMapV1AnyPortReturnsConflict 覆盖方案要求：external==0（网关任选端口）在
// IGDv1/PPP 上没有对应动作，直接给 ErrConflict 交给 Mapper 决定是否换固定端口重试。
func TestUPnPMapV1AnyPortReturnsConflict(t *testing.T) {
	u := &upnpClient{conn: &fakeConn{sc: newLoopbackServiceClient(t)}} // v2 == nil
	_, err := u.Map(context.Background(), Want{Proto: "udp", Port: 47700, Desc: "hearth test"}, 0, time.Hour)
	if !errors.Is(err, ErrConflict) {
		t.Fatalf("Map(external=0) on v1 = %v, want ErrConflict", err)
	}
}

// fakeIGDv1 是一个跑在 httptest 之上的最小 IGDv1 设备：只服务 WANIPConnection:1
// 的设备描述与三个动作（AddPortMapping/DeletePortMapping/GetExternalIPAddress），
// 用来验证 upnp.go 与 goupnp 生成代码之间的往返打包/解析没有走偏。
type fakeIGDv1 struct {
	mu       sync.Mutex
	added    []addCall
	deleted  []deleteCall
	extIP    string
	failNext string // 下一次匹配该动作名的调用返回 714 (NoSuchEntryInArray)
}

type addCall struct {
	externalPort   int
	internalPort   int
	protocol       string
	description    string
	internalClient string
	lease          int
}

type deleteCall struct {
	externalPort int
	protocol     string
}

const wanIPConnV1NS = "urn:schemas-upnp-org:service:WANIPConnection:1"

func (f *fakeIGDv1) handler() http.HandlerFunc {
	const deviceXML = `<?xml version="1.0"?>
<root xmlns="urn:schemas-upnp-org:device-1-0">
  <specVersion><major>1</major><minor>0</minor></specVersion>
  <device>
    <deviceType>urn:schemas-upnp-org:device:InternetGatewayDevice:1</deviceType>
    <friendlyName>fake igd</friendlyName>
    <manufacturer>hearth-test</manufacturer>
    <modelName>fake</modelName>
    <UDN>uuid:fake-igd-1</UDN>
    <deviceList>
      <device>
        <deviceType>urn:schemas-upnp-org:device:WANDevice:1</deviceType>
        <friendlyName>WAN Device</friendlyName>
        <manufacturer>hearth-test</manufacturer>
        <modelName>fake</modelName>
        <UDN>uuid:fake-wan-1</UDN>
        <deviceList>
          <device>
            <deviceType>urn:schemas-upnp-org:device:WANConnectionDevice:1</deviceType>
            <friendlyName>WAN Connection Device</friendlyName>
            <manufacturer>hearth-test</manufacturer>
            <modelName>fake</modelName>
            <UDN>uuid:fake-wanconn-1</UDN>
            <serviceList>
              <service>
                <serviceType>urn:schemas-upnp-org:service:WANIPConnection:1</serviceType>
                <serviceId>urn:upnp-org:serviceId:WANIPConn1</serviceId>
                <controlURL>/ctl</controlURL>
                <eventSubURL>/evt</eventSubURL>
                <SCPDURL>/scpd.xml</SCPDURL>
              </service>
            </serviceList>
          </device>
        </deviceList>
      </device>
    </deviceList>
  </device>
</root>`

	return func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/desc.xml":
			w.Header().Set("Content-Type", "text/xml")
			_, _ = w.Write([]byte(deviceXML))
		case r.Method == http.MethodPost && r.URL.Path == "/ctl":
			f.serveSOAP(w, r)
		default:
			http.NotFound(w, r)
		}
	}
}

func (f *fakeIGDv1) serveSOAP(w http.ResponseWriter, r *http.Request) {
	action := r.Header.Get("SOAPACTION")
	body, _ := io.ReadAll(r.Body)
	switch {
	case strings.Contains(action, "AddPortMapping") && !strings.Contains(action, "AddAnyPortMapping"):
		ext := extractIntTag(body, "NewExternalPort")
		in := extractIntTag(body, "NewInternalPort")
		lease := extractIntTag(body, "NewLeaseDuration")
		proto := extractStringTag(body, "NewProtocol")
		desc := extractStringTag(body, "NewPortMappingDescription")
		ic := extractStringTag(body, "NewInternalClient")
		f.mu.Lock()
		f.added = append(f.added, addCall{externalPort: ext, internalPort: in, protocol: proto, description: desc, internalClient: ic, lease: lease})
		f.mu.Unlock()
		writeSOAPResult(w, wanIPConnV1NS, "AddPortMapping", "")
	case strings.Contains(action, "DeletePortMapping"):
		ext := extractIntTag(body, "NewExternalPort")
		proto := extractStringTag(body, "NewProtocol")
		f.mu.Lock()
		shouldFail := f.failNext == "DeletePortMapping"
		f.failNext = ""
		f.deleted = append(f.deleted, deleteCall{externalPort: ext, protocol: proto})
		f.mu.Unlock()
		if shouldFail {
			writeSOAPFault(w, 714, "NoSuchEntryInArray")
			return
		}
		writeSOAPResult(w, wanIPConnV1NS, "DeletePortMapping", "")
	case strings.Contains(action, "GetExternalIPAddress"):
		writeSOAPResult(w, wanIPConnV1NS, "GetExternalIPAddress",
			"<NewExternalIPAddress>"+f.extIP+"</NewExternalIPAddress>")
	default:
		http.Error(w, "unknown action: "+action, http.StatusNotImplemented)
	}
}

func writeSOAPResult(w http.ResponseWriter, ns, action, innerXML string) {
	w.Header().Set("Content-Type", "text/xml; charset=\"utf-8\"")
	fmt.Fprintf(w, `<?xml version="1.0"?>
<s:Envelope xmlns:s="http://schemas.xmlsoap.org/soap/envelope/" s:encodingStyle="http://schemas.xmlsoap.org/soap/encoding/">
<s:Body><u:%sResponse xmlns:u=%q>%s</u:%sResponse></s:Body>
</s:Envelope>`, action, ns, innerXML, action)
}

func writeSOAPFault(w http.ResponseWriter, code int, desc string) {
	w.Header().Set("Content-Type", "text/xml; charset=\"utf-8\"")
	w.WriteHeader(http.StatusInternalServerError)
	fmt.Fprintf(w, `<?xml version="1.0"?>
<s:Envelope xmlns:s="http://schemas.xmlsoap.org/soap/envelope/" s:encodingStyle="http://schemas.xmlsoap.org/soap/encoding/">
<s:Body><s:Fault>
<faultcode>s:Client</faultcode>
<faultstring>UPnPError</faultstring>
<detail><UPnPError xmlns="urn:schemas-upnp-org:control-1-0">
<errorCode>%d</errorCode>
<errorDescription>%s</errorDescription>
</UPnPError></detail>
</s:Fault></s:Body>
</s:Envelope>`, code, desc)
}

// extractIntTag/extractStringTag 是够用就好的 XML 取值：请求体是 upnp.go 自己拼的，
// 标签集固定，不必引入完整 XML 解析。
func extractIntTag(body []byte, tag string) int {
	s := extractStringTag(body, tag)
	var v int
	_, _ = fmt.Sscanf(s, "%d", &v)
	return v
}

func extractStringTag(body []byte, tag string) string {
	open := "<" + tag + ">"
	shut := "</" + tag + ">"
	s := string(body)
	i := strings.Index(s, open)
	if i < 0 {
		return ""
	}
	s = s[i+len(open):]
	j := strings.Index(s, shut)
	if j < 0 {
		return ""
	}
	return s[:j]
}

// TestUPnPv1RoundTrip 用 httptest 起一个只服务 WANIPConnection:1 的假 IGD，
// 绕开 SSDP 发现（直接用 ByURL 变体拿 client），验证 Map/Unmap 与 goupnp
// 生成代码之间的请求打包、响应解析、714 视为成功都按预期工作。
func TestUPnPv1RoundTrip(t *testing.T) {
	fake := &fakeIGDv1{extIP: "203.0.113.5"} // TEST-NET-3，仅测试占位，非真实地址
	srv := httptest.NewServer(fake.handler())
	defer srv.Close()

	descURL, err := url.Parse(srv.URL + "/desc.xml")
	if err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	clients, err := internetgateway2.NewWANIPConnection1ClientsByURLCtx(ctx, descURL)
	if err != nil {
		t.Fatalf("NewWANIPConnection1ClientsByURLCtx: %v", err)
	}
	if len(clients) != 1 {
		t.Fatalf("got %d WANIPConnection1 clients, want 1", len(clients))
	}

	u := &upnpClient{conn: clients[0]} // v1：v2 留空

	want := Want{Proto: "udp", Port: 47700, Desc: "hearth test"}
	mapping, err := u.Map(ctx, want, 30000, time.Hour)
	if err != nil {
		t.Fatalf("Map: %v", err)
	}
	if mapping.External != 30000 || mapping.Internal != 47700 || mapping.Method != "upnp" {
		t.Fatalf("unexpected mapping: %+v", mapping)
	}
	if mapping.ExternalIP.String() != "203.0.113.5" {
		t.Fatalf("mapping.ExternalIP = %v, want 203.0.113.5", mapping.ExternalIP)
	}
	if mapping.ExpiresAt.Before(time.Now().Add(30 * time.Minute)) {
		t.Fatalf("mapping.ExpiresAt = %v, want roughly one hour out", mapping.ExpiresAt)
	}

	fake.mu.Lock()
	if len(fake.added) != 1 || fake.added[0].protocol != "UDP" || fake.added[0].externalPort != 30000 || fake.added[0].internalPort != 47700 {
		fake.mu.Unlock()
		t.Fatalf("unexpected AddPortMapping call recorded: %+v", fake.added)
	}
	fake.mu.Unlock()

	if err := u.Unmap(ctx, want, 30000); err != nil {
		t.Fatalf("Unmap: %v", err)
	}
	fake.mu.Lock()
	if len(fake.deleted) != 1 || fake.deleted[0].externalPort != 30000 {
		fake.mu.Unlock()
		t.Fatalf("unexpected DeletePortMapping call recorded: %+v", fake.deleted)
	}
	fake.mu.Unlock()

	// 714 NoSuchEntryInArray 视为已删除，不是错误。
	fake.mu.Lock()
	fake.failNext = "DeletePortMapping"
	fake.mu.Unlock()
	if err := u.Unmap(ctx, want, 30000); err != nil {
		t.Fatalf("Unmap after 714 = %v, want nil (already gone counts as success)", err)
	}
}

// TestLocalAddrFor 验证内部客户端地址取的是"面向设备出口"的地址，而不是随便一块
// 网卡：用回环地址做目标，出口地址理应仍在回环网段。
func TestUPnPLocalAddrFor(t *testing.T) {
	loc := &url.URL{Scheme: "http", Host: "127.0.0.1:1900"}
	addr, err := localAddrFor(loc)
	if err != nil {
		t.Fatalf("localAddrFor: %v", err)
	}
	if !addr.IsLoopback() {
		t.Fatalf("localAddrFor(127.0.0.1) = %v, want a loopback address", addr)
	}
}

func newLoopbackServiceClient(t *testing.T) *goupnp.ServiceClient {
	t.Helper()
	return &goupnp.ServiceClient{Location: &url.URL{Scheme: "http", Host: "127.0.0.1:1900"}}
}

// TestLiveUPnPGateway 仅在设置 PORTMAP_LIVE=1 时运行：对着开发者局域网里真实存在的
// 网关跑一次 discover -> Map -> Unmap，用于人工验收（详见开发笔记，地址与设备型号
// 不写进仓库）。CI 与默认 `go test` 不会触发。
func TestLiveUPnPGateway(t *testing.T) {
	if os.Getenv("PORTMAP_LIVE") != "1" {
		t.Skip("set PORTMAP_LIVE=1 to run against a real gateway on the local network")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	c, err := discoverUPnP(ctx)
	if err != nil {
		t.Fatalf("discoverUPnP: %v", err)
	}
	if c.Method() != "upnp" {
		t.Fatalf("Method() = %q, want upnp", c.Method())
	}

	want := Want{Proto: "udp", Port: 47700, Desc: "hearth portmap live test"}
	mapping, err := c.Map(ctx, want, 47700, time.Minute)
	if err != nil {
		t.Fatalf("Map: %v", err)
	}
	t.Logf("live mapping established: proto=%s internal=%d external=%d method=%s expires=%s",
		mapping.Proto, mapping.Internal, mapping.External, mapping.Method, mapping.ExpiresAt)

	if err := c.Unmap(ctx, want, mapping.External); err != nil {
		t.Fatalf("Unmap: %v", err)
	}
}
