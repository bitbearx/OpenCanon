package bjnp

import (
	"context"
	"encoding/binary"
	"errors"
	"strings"
	"testing"
	"time"
)

// e560IdleXML 保存 E560 实机返回的完整状态信封与原始 BSCCe 数据。
const e560IdleXML = `<?xml version="1.0" encoding="utf-8" ?>
<cmd xmlns:ivec="http://www.canon.com/ns/cmd/2008/07/common/"
xmlns:cijn="http://www.canon.com/ns/wdp/2008/01/print/">
	<ivec:contents>
		<ivec:operation>GetStatusResponse</ivec:operation>
		<ivec:param_set servicetype="print">
			<ivec:response>OK</ivec:response>
			<ivec:response_detail/>
			<ivec:status>idle</ivec:status>
			<ivec:status_detail/>
			<cijn:IJPrinterStatus>
				<cijn:Name>BSCCe</cijn:Name>
				<cijn:ReadData>BST:00;PID:00,00,00;CHD:CL,BKX;LVR:GAL,AT;MID:A2599D;CTK:CL,SET,/,BK,SET;CIR:CL=020,BK=050;DJS:NO;DBS:NO;DWS:NO;DOC:4,00,NO;DSC:NO;FSI:00;HRI:18;MSI:AOFF,B240,DAT,E3,HFSF,JOFF,K21000700,LOFF;PDR:7;HRC:NO;TCA:CL,AC,/,BK,AC;DSI:21,0001,/,31,0001,/,2F,0048,/,3F,008B;RRC:DR;</cijn:ReadData>
				<cijn:NumberOfBytes>271</cijn:NumberOfBytes>
			</cijn:IJPrinterStatus>
			<ivec:current_support_code/>
		</ivec:param_set>
	</ivec:contents>
</cmd>`

// TestProbeE560XML 验证实机状态、忙碌及错误均通过公开探测与提交前检查解释。
func TestProbeE560XML(t *testing.T) {
	cases := []struct {
		name, old, replacement string
		busy, deviceError      bool
	}{
		{name: "captured-idle"},
		{name: "idle-without-declaration", old: "<?xml version=\"1.0\" encoding=\"utf-8\" ?>", replacement: ""},
		{name: "busy", old: ">idle<", replacement: ">busy<", busy: true},
		{name: "captured-processing", old: "idle</ivec:status>", replacement: "processing</ivec:status><ivec:jobstatus><ivec:jobID>00000001</ivec:jobID><ivec:jobprogress>processing</ivec:jobprogress></ivec:jobstatus>", busy: true},
		{name: "error", old: ">idle<", replacement: ">error<", deviceError: true},
		{name: "idle-with-bst-busy", old: "BST:00;", replacement: "BST:80;", busy: true},
		{name: "idle-with-bst-error", old: "BST:00;", replacement: "BST:08;", deviceError: true},
		{name: "idle-with-paper-out", old: "DJS:NO;", replacement: "DJS:ID;", deviceError: true},
		{name: "idle-with-ink-out", old: "CTK:CL,SET,/,BK,SET;", replacement: "CTK:CL,IO ,/,BK,SET;", deviceError: true},
		{name: "idle-with-support-code", old: "<ivec:current_support_code/>", replacement: "<ivec:current_support_code>1300</ivec:current_support_code>", deviceError: true},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			payload := e560IdleXML
			if test.old != "" {
				payload = strings.Replace(payload, test.old, test.replacement, 1)
			}
			device := newTestDevice(t, func(command byte, response []byte) []byte {
				if command == cmdStatus {
					return responsePacket(command, 1, binary.BigEndian.Uint16(response[10:]), []byte(payload))
				}
				return response
			}, nil)
			client := Client{Address: device.address, Timeout: time.Second}
			info, err := client.Probe(context.Background())
			if err != nil || info.Busy != test.busy || info.DeviceError != test.deviceError || info.Model != "E560 series" || info.Status != payload {
				t.Fatalf("info=%+v err=%v", info, err)
			}
			if test.busy || test.deviceError {
				_, err := client.Submit(context.Background(), strings.NewReader("must not print"), "blocked")
				if err == nil || errors.Is(err, ErrUncertain) || errors.Is(err, ErrBusy) != test.busy {
					t.Fatalf("unexpected preflight error: %v", err)
				}
			}
			if device.starts.Load() != 0 {
				t.Fatal("probe or failed preflight opened a print session")
			}
		})
	}
}

// TestProbeRejectsAmbiguousE560XML 验证未知、截断及结构歧义不会开启打印会话。
func TestProbeRejectsAmbiguousE560XML(t *testing.T) {
	cases := []struct {
		name, old, replacement string
	}{
		{"common-namespace", "http://www.canon.com/ns/cmd/2008/07/common/", "urn:untrusted"},
		{"print-namespace", "http://www.canon.com/ns/wdp/2008/01/print/", "urn:untrusted"},
		{"root-namespace", "<cmd ", "<cmd xmlns=\"urn:untrusted\" "},
		{"unqualified-status", "<ivec:status>idle</ivec:status>", "<status>idle</status>"},
		{"wrong-root", "cmd", "envelope"},
		{"wrong-operation", "GetStatusResponse", "SetConfigurationResponse"},
		{"wrong-service", "servicetype=\"print\"", "servicetype=\"scan\""},
		{"missing-service", "servicetype=\"print\"", ""},
		{"duplicate-service", "servicetype=\"print\"", "servicetype=\"scan\" servicetype=\"print\""},
		{"unknown-attribute", "servicetype=\"print\"", "servicetype=\"print\" status=\"idle\""},
		{"response-error", ">OK<", ">NG<"},
		{"response-detail", "<ivec:response_detail/>", "<ivec:response_detail>error</ivec:response_detail>"},
		{"status-detail", "<ivec:status_detail/>", "<ivec:status_detail>error</ivec:status_detail>"},
		{"unknown-status", ">idle<", ">unknown<"},
		{"unconfirmed-processing", ">idle<", ">processing<"},
		{"unconfirmed-stopped", ">idle<", ">stopped<"},
		{"unconfirmed-notready", ">idle<", ">notready<"},
		{"unconfirmed-canceling", ">idle<", ">canceling<"},
		{"missing-status", "<ivec:status>idle</ivec:status>", ""},
		{"duplicate-status", "<ivec:status>idle</ivec:status>", "<ivec:status>error</ivec:status><ivec:status>idle</ivec:status>"},
		{"duplicate-operation", "<ivec:operation>GetStatusResponse</ivec:operation>", "<ivec:operation>Other</ivec:operation><ivec:operation>GetStatusResponse</ivec:operation>"},
		{"duplicate-response", "<ivec:response>OK</ivec:response>", "<ivec:response>NG</ivec:response><ivec:response>OK</ivec:response>"},
		{"duplicate-name", "<cijn:Name>BSCCe</cijn:Name>", "<cijn:Name>Other</cijn:Name><cijn:Name>BSCCe</cijn:Name>"},
		{"wrong-name", ">BSCCe<", ">Other<"},
		{"missing-name", "<cijn:Name>BSCCe</cijn:Name>", ""},
		{"missing-data", "cijn:ReadData", "cijn:OtherData"},
		{"missing-size", "<cijn:NumberOfBytes>271</cijn:NumberOfBytes>", ""},
		{"wrong-size", ">271<", ">270<"},
		{"duplicate-size", "<cijn:NumberOfBytes>271</cijn:NumberOfBytes>", "<cijn:NumberOfBytes>0</cijn:NumberOfBytes><cijn:NumberOfBytes>271</cijn:NumberOfBytes>"},
		{"duplicate-read-data", "</cijn:ReadData>", "</cijn:ReadData><cijn:ReadData>BST:00;</cijn:ReadData>"},
		{"missing-support-code", "<ivec:current_support_code/>", ""},
		{"duplicate-printer-status", "</cijn:IJPrinterStatus>", "</cijn:IJPrinterStatus><cijn:IJPrinterStatus/>"},
		{"duplicate-support-code", "<ivec:current_support_code/>", "<ivec:current_support_code>1300</ivec:current_support_code><ivec:current_support_code/>"},
		{"bad-support-code", "<ivec:current_support_code/>", "<ivec:current_support_code>unknown</ivec:current_support_code>"},
		{"wrong-path", "<ivec:status>idle</ivec:status>", "<ivec:wrapper><ivec:status>idle</ivec:status></ivec:wrapper>"},
		{"nested-text", ">idle<", "><ivec:text>idle</ivec:text><"},
		{"container-text", "<ivec:contents>", "<ivec:contents>unexpected"},
		{"duplicate-param-set", "</ivec:param_set>", "</ivec:param_set><ivec:param_set servicetype=\"print\"/>"},
		{"duplicate-contents", "</ivec:contents>", "</ivec:contents><ivec:contents/>"},
		{"duplicate-envelope", "</cmd>", "</cmd><cmd/>"},
		{"trailing-data", "</cmd>", "</cmd>garbage"},
		{"truncated", "</cmd>", ""},
		{"directive", "<cmd ", "<!DOCTYPE cmd><cmd "},
		{"missing-bst", "BST:00;", "XYZ:00;"},
		{"bad-bst", "BST:00;", "BST:zz;"},
		{"duplicate-bst", "PID:00,00,00;", "BST:00;      "},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			payload := strings.Replace(e560IdleXML, test.old, test.replacement, 1)
			device := newTestDevice(t, func(command byte, response []byte) []byte {
				if command == cmdStatus {
					return responsePacket(command, 1, 0, []byte(payload))
				}
				return response
			}, nil)
			client := Client{Address: device.address, Timeout: time.Second}
			if info, err := client.Probe(context.Background()); err == nil {
				t.Fatalf("accepted unknown or malformed status: %+v", info)
			}
			if _, err := client.Submit(context.Background(), strings.NewReader("must not print"), "blocked"); err == nil || errors.Is(err, ErrUncertain) {
				t.Fatalf("unexpected preflight error: %v", err)
			}
			if device.starts.Load() != 0 {
				t.Fatal("invalid status opened a print session")
			}
		})
	}
}
