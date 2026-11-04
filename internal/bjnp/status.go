package bjnp

import (
	"bytes"
	"encoding/hex"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"strconv"
	"strings"
)

// parseStatus 区分完整 XML 信封与旧式状态；未知或不完整状态不报告为就绪。
func parseStatus(info *Info, payload []byte) error {
	if data := bytes.TrimSpace(payload); len(data) != 0 && data[0] == '<' {
		info.Status = string(payload)
		return parseXMLStatus(info, payload)
	}
	if len(payload) < 3 {
		return errors.New("BJNP 状态负载不完整")
	}
	// 公开实现没有定义旧状态内层长度是否包含自身，因此只使用已校验的外层长度。
	info.Status = strings.TrimRight(string(payload[2:]), "\x00")
	return parseStatusFields(info, info.Status)
}

// parseStatusFields 统一解释旧状态与 BSCCe ReadData 中的 BST、缺纸和耗材错误。
func parseStatusFields(info *Info, text string) error {
	values, err := fields(text)
	if err != nil {
		return err
	}
	bst := values["BST"]
	if len(bst) < 2 {
		return errors.New("BJNP 状态缺少有效 BST 字段，无法判定就绪")
	}
	for _, digit := range bst {
		if !((digit >= '0' && digit <= '9') || (digit >= 'a' && digit <= 'f') || (digit >= 'A' && digit <= 'F')) {
			return errors.New("BJNP BST 状态包含非十六进制数据")
		}
	}
	var status [1]byte
	if _, err := hex.Decode(status[:], []byte(bst[:2])); err != nil {
		return errors.New("BJNP BST 状态位无效")
	}
	info.Busy = status[0]&(0x80|0x20) != 0
	info.DeviceError = status[0]&0x08 != 0 || values["DJS"] == "ID"
	for _, token := range strings.FieldsFunc(values["CTK"], func(r rune) bool { return r == ',' || r == ' ' || r == '=' }) {
		if token == "IO" {
			info.DeviceError = true
		}
	}
	return nil
}

const (
	statusCommand = iota
	statusContents
	statusOperation
	statusParameters
	statusResponse
	statusResponseDetail
	statusState
	statusStateDetail
	statusPrinter
	statusName
	statusReadData
	statusByteCount
	statusSupportCode
	statusJob
	statusJobID
	statusJobProgress
)

// statusXMLNode 限定节点的命名空间与直接父节点，并记录是否已出现以拒绝重复值。
type statusXMLNode struct {
	parent    int
	name      xml.Name
	container bool
	seen      bool
	optional  bool
	value     string
}

// parseXMLStatus 只接受已确认的打印状态信封，所有必需节点必须各出现一次。
func parseXMLStatus(info *Info, payload []byte) error {
	const common = "http://www.canon.com/ns/cmd/2008/07/common/"
	const print = "http://www.canon.com/ns/wdp/2008/01/print/"
	nodes := [...]statusXMLNode{
		{parent: -1, name: xml.Name{Local: "cmd"}, container: true},
		{parent: statusCommand, name: xml.Name{Space: common, Local: "contents"}, container: true},
		{parent: statusContents, name: xml.Name{Space: common, Local: "operation"}},
		{parent: statusContents, name: xml.Name{Space: common, Local: "param_set"}, container: true},
		{parent: statusParameters, name: xml.Name{Space: common, Local: "response"}},
		{parent: statusParameters, name: xml.Name{Space: common, Local: "response_detail"}},
		{parent: statusParameters, name: xml.Name{Space: common, Local: "status"}},
		{parent: statusParameters, name: xml.Name{Space: common, Local: "status_detail"}},
		{parent: statusParameters, name: xml.Name{Space: print, Local: "IJPrinterStatus"}, container: true},
		{parent: statusPrinter, name: xml.Name{Space: print, Local: "Name"}},
		{parent: statusPrinter, name: xml.Name{Space: print, Local: "ReadData"}},
		{parent: statusPrinter, name: xml.Name{Space: print, Local: "NumberOfBytes"}},
		{parent: statusParameters, name: xml.Name{Space: common, Local: "current_support_code"}},
		{parent: statusParameters, name: xml.Name{Space: common, Local: "jobstatus"}, container: true, optional: true},
		{parent: statusJob, name: xml.Name{Space: common, Local: "jobID"}, optional: true},
		{parent: statusJob, name: xml.Name{Space: common, Local: "jobprogress"}, optional: true},
	}
	decoder := xml.NewDecoder(bytes.NewReader(payload))
	var stack [5]int
	var text strings.Builder
	depth := 0
	declarationSeen := false
	for {
		token, err := decoder.Token()
		if err == io.EOF {
			break
		}
		if err != nil {
			return fmt.Errorf("BJNP XML 状态格式无效: %w", err)
		}
		switch token := token.(type) {
		case xml.StartElement:
			parent := -1
			if depth != 0 {
				parent = stack[depth-1]
			}
			index := -1
			for i := range nodes {
				if nodes[i].parent == parent && nodes[i].name == token.Name {
					index = i
					break
				}
			}
			if index < 0 || depth == len(stack) {
				return fmt.Errorf("BJNP XML 状态节点或路径无效: %s", token.Name.Local)
			}
			if nodes[index].seen {
				return fmt.Errorf("BJNP XML 状态节点重复: %s", token.Name.Local)
			}
			serviceSeen := false
			for i, attr := range token.Attr {
				for _, previous := range token.Attr[:i] {
					if previous.Name == attr.Name {
						return fmt.Errorf("BJNP XML 状态属性重复: %s", attr.Name.Local)
					}
				}
				if attr.Name.Space == "xmlns" || (attr.Name.Space == "" && attr.Name.Local == "xmlns") {
					continue
				}
				if index != statusParameters || attr.Name.Space != "" || attr.Name.Local != "servicetype" || attr.Value != "print" {
					return fmt.Errorf("BJNP XML 状态属性无效: %s", attr.Name.Local)
				}
				serviceSeen = true
			}
			if index == statusParameters && !serviceSeen {
				return errors.New("BJNP XML 状态缺少 print 服务类型")
			}
			nodes[index].seen = true
			stack[depth] = index
			depth++
			if !nodes[index].container {
				text.Reset()
			}
		case xml.EndElement:
			if depth == 0 || nodes[stack[depth-1]].name != token.Name {
				return errors.New("BJNP XML 状态结束节点无效")
			}
			index := stack[depth-1]
			if !nodes[index].container {
				nodes[index].value = text.String()
			}
			depth--
		case xml.CharData:
			if depth == 0 || nodes[stack[depth-1]].container {
				if len(bytes.TrimSpace(token)) != 0 {
					return errors.New("BJNP XML 状态信封含意外文本")
				}
			} else {
				text.Write(token)
			}
		case xml.ProcInst:
			if token.Target != "xml" || declarationSeen || nodes[statusCommand].seen {
				return errors.New("BJNP XML 状态处理指令无效")
			}
			declarationSeen = true
		case xml.Directive:
			return errors.New("BJNP XML 状态不接受声明指令")
		}
	}
	for _, node := range nodes {
		if !node.seen && (!node.optional || (node.parent == statusJob && nodes[statusJob].seen)) {
			return fmt.Errorf("BJNP XML 状态缺少节点: %s", node.name.Local)
		}
	}
	if depth != 0 || strings.TrimSpace(nodes[statusOperation].value) != "GetStatusResponse" || strings.TrimSpace(nodes[statusResponse].value) != "OK" || strings.TrimSpace(nodes[statusName].value) != "BSCCe" {
		return errors.New("BJNP XML 状态操作、响应或 BSCCe 标识无效")
	}
	if strings.TrimSpace(nodes[statusResponseDetail].value) != "" || strings.TrimSpace(nodes[statusStateDetail].value) != "" {
		return errors.New("BJNP XML 状态包含未识别的详细状态")
	}
	data := nodes[statusReadData].value
	if strings.TrimSpace(nodes[statusByteCount].value) != strconv.Itoa(len(data)) {
		return errors.New("BJNP XML BSCCe 数据长度无效")
	}
	state := strings.TrimSpace(nodes[statusState].value)
	if state != "idle" && state != "busy" && state != "processing" && state != "error" {
		return fmt.Errorf("BJNP XML 状态未知: %s", state)
	}
	if (state == "processing") != nodes[statusJob].seen || (nodes[statusJob].seen && (strings.TrimSpace(nodes[statusJobID].value) == "" || strings.TrimSpace(nodes[statusJobProgress].value) != "processing")) {
		return errors.New("BJNP XML 作业状态未知或与设备状态矛盾")
	}
	support := strings.TrimSpace(nodes[statusSupportCode].value)
	for _, digit := range support {
		if digit < '0' || digit > '9' {
			return errors.New("BJNP XML 支持代码格式无效")
		}
	}
	if err := parseStatusFields(info, data); err != nil {
		return err
	}
	// 信封只增加阻塞条件，idle 不能清除 BST 或耗材报告的错误。
	info.Busy = info.Busy || state == "busy" || state == "processing"
	info.DeviceError = info.DeviceError || state == "error" || support != ""
	return nil
}
