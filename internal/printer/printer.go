// Package printer 校验 E560 能力并通过独立编码器和 BJNP 提交任务。
package printer

import (
	"encoding/json"
	"errors"
	"io"
	"math"
	"os"
	"strings"

	"github.com/bitbearx/opencanon/internal/render"
)

const Name = "Canon E560 series"

// Media 描述设备编码器使用的纸张尺寸和介质类型。
type Media struct {
	ID       string  `json:"id"`
	Label    string  `json:"label"`
	WidthMM  float64 `json:"widthMM"`
	HeightMM float64 `json:"heightMM"`
	Type     string  `json:"type"`
}

// Mode 描述用户可选的颜色或双面模式。
type Mode struct {
	ID    string `json:"id"`
	Label string `json:"label"`
}

// Constraint 描述不允许的打印设置组合。
type Constraint struct {
	MediaID   string `json:"mediaId"`
	ColorMode string `json:"colorMode"`
	Duplex    string `json:"duplex"`
}

// Profile 保存精确 E560 型号的独立编码能力。
type Profile struct {
	Model       string       `json:"model"`
	Media       []Media      `json:"media"`
	ColorModes  []Mode       `json:"colorModes"`
	DuplexModes []Mode       `json:"duplexModes"`
	Constraints []Constraint `json:"constraints"`
}

// Load 读取配置并拒绝未知字段和额外 JSON。
func Load(path string) (*Profile, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	var p Profile
	dec := json.NewDecoder(f)
	dec.DisallowUnknownFields()
	if err := dec.Decode(&p); err != nil {
		return nil, err
	}
	var extra any
	if dec.Decode(&extra) != io.EOF {
		return nil, errors.New("extra JSON")
	}
	if err := p.Validate(); err != nil {
		return nil, err
	}
	return &p, nil
}

// Validate 验证型号、纸张尺寸、模式和约束的引用完整性。
func (p *Profile) Validate() error {
	if p.Model != "bjc-E560-series" || len(p.Media) == 0 || len(p.ColorModes) == 0 || len(p.DuplexModes) == 0 {
		return errors.New("invalid E560 profile")
	}
	media, colors, duplex := map[string]bool{}, map[string]bool{}, map[string]bool{}
	for _, m := range p.Media {
		if m.ID == "" || strings.ContainsAny(m.ID, "/\\\x00") || media[m.ID] || m.Label == "" || math.IsNaN(m.WidthMM) || math.IsInf(m.WidthMM, 0) || m.WidthMM <= 0 || m.WidthMM > 216 || math.IsNaN(m.HeightMM) || math.IsInf(m.HeightMM, 0) || m.HeightMM <= 0 || m.HeightMM > 356 {
			return errors.New("invalid media")
		}
		switch m.Type {
		case "plain", "photo", "postcard", "envelope":
		default:
			return errors.New("invalid media type")
		}
		media[m.ID] = true
	}
	for _, m := range p.ColorModes {
		if (m.ID != "color" && m.ID != "mono") || colors[m.ID] || m.Label == "" {
			return errors.New("invalid color")
		}
		colors[m.ID] = true
	}
	for _, m := range p.DuplexModes {
		if (m.ID != "single" && m.ID != "long" && m.ID != "short") || duplex[m.ID] || m.Label == "" {
			return errors.New("invalid duplex")
		}
		duplex[m.ID] = true
	}
	if !duplex["single"] {
		return errors.New("single missing")
	}
	for _, c := range p.Constraints {
		if !media[c.MediaID] || !colors[c.ColorMode] || !duplex[c.Duplex] {
			return errors.New("invalid constraint")
		}
	}
	return nil
}

// Options 将通过校验的选择转换为独立渲染参数。
func (p *Profile) Options(mediaID, color, duplex string) (render.Job, error) {
	job := render.Job{Media: mediaID, ColorMode: color, Duplex: duplex}
	foundMedia, foundColor, foundDuplex := false, false, false
	for _, m := range p.Media {
		if m.ID == mediaID {
			job.MediaType, job.WidthMM, job.HeightMM = m.Type, m.WidthMM, m.HeightMM
			foundMedia = true
		}
	}
	for _, m := range p.ColorModes {
		if m.ID == color {
			foundColor = true
		}
	}
	for _, m := range p.DuplexModes {
		if m.ID == duplex {
			foundDuplex = true
		}
	}
	if !foundMedia || !foundColor || !foundDuplex {
		return job, errors.New("invalid selection")
	}
	if duplex != "single" && (job.MediaType != "plain" || (mediaID != "A4" && mediaID != "Letter")) {
		return job, errors.New("unsupported duplex media")
	}
	for _, c := range p.Constraints {
		if c.MediaID == mediaID && c.ColorMode == color && c.Duplex == duplex {
			return job, errors.New("forbidden combination")
		}
	}
	return job, nil
}

// Sheets 按逐份排序计算奇数页双面所需的纸张。
func Sheets(pages, copies int, duplex string) int {
	if duplex != "single" {
		pages = (pages + 1) / 2
	}
	return pages * copies
}
