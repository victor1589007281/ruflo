// 可编辑 PPTX 生成器: 把结构化 slide 数据生成为含真实文本框(标题/正文要点)的 .pptx。
//
// 与 pptx.go 的差异:
//   - pptx.go (buildSimplePPTX): 每页一张整页截图当背景图 → 高保真但**不可编辑**。
//   - 本文件 (BuildEditablePPTX): 每页生成真正的 <p:sp> 文本形状(标题+正文要点) → 在
//     PowerPoint / WPS / Keynote 里可直接改字、调样式。可选附带每页背景图。
//
// 复用 pptx.go 已验证的包骨架(Content_Types / rels / slideMaster / slideLayout), 仅把
// slide 正文从整页图换成文本形状, 最大化兼容性。纯 Go, 无外部依赖。
package media

import (
	"archive/zip"
	"fmt"
	"os"
	"strings"
)

// EditableSlide 一页可编辑幻灯片。
type EditableSlide struct {
	Title    string   `json:"title"`              // 标题文本框
	Bullets  []string `json:"bullets,omitempty"`  // 正文要点 (每条一段)
	Subtitle string   `json:"subtitle,omitempty"` // 副标题 (可选, 接在标题下)
	Notes    string   `json:"notes,omitempty"`    // 演讲备注 (写入 sidecar .notes.md)
	ImagePNG string   `json:"image_png,omitempty"`// 可选: 该页背景图的本地 PNG 路径
}

// EditableDeck 一份可编辑演示文稿。
type EditableDeck struct {
	Title  string          `json:"title,omitempty"`
	Slides []EditableSlide `json:"slides"`
}

// BuildEditablePPTX 生成可编辑 PPTX。若任何 slide 带 Notes, 额外写出 <outPath>.notes.md。
func BuildEditablePPTX(outPath string, deck EditableDeck) error {
	if len(deck.Slides) == 0 {
		return fmt.Errorf("无幻灯片内容")
	}

	f, err := os.Create(outPath)
	if err != nil {
		return err
	}
	defer f.Close()
	w := zip.NewWriter(f)
	defer w.Close()

	// [Content_Types].xml
	ct := `<?xml version="1.0" encoding="UTF-8" standalone="yes"?>
<Types xmlns="http://schemas.openxmlformats.org/package/2006/content-types">
  <Default Extension="rels" ContentType="application/vnd.openxmlformats-package.relationships+xml"/>
  <Default Extension="xml" ContentType="application/xml"/>
  <Default Extension="png" ContentType="image/png"/>
  <Override PartName="/ppt/presentation.xml" ContentType="application/vnd.openxmlformats-officedocument.presentationml.presentation.main+xml"/>
  <Override PartName="/ppt/slideMasters/slideMaster1.xml" ContentType="application/vnd.openxmlformats-officedocument.presentationml.slideMaster+xml"/>
  <Override PartName="/ppt/slideLayouts/slideLayout1.xml" ContentType="application/vnd.openxmlformats-officedocument.presentationml.slideLayout+xml"/>`
	for i := range deck.Slides {
		ct += fmt.Sprintf(`
  <Override PartName="/ppt/slides/slide%d.xml" ContentType="application/vnd.openxmlformats-officedocument.presentationml.slide+xml"/>`, i+1)
	}
	ct += "\n</Types>"
	addFile(w, "[Content_Types].xml", ct)

	addFile(w, "_rels/.rels", `<?xml version="1.0" encoding="UTF-8" standalone="yes"?>
<Relationships xmlns="http://schemas.openxmlformats.org/package/2006/relationships">
  <Relationship Id="rId1" Type="http://schemas.openxmlformats.org/officeDocument/2006/relationships/officeDocument" Target="ppt/presentation.xml"/>
</Relationships>`)

	// presentation.xml
	var slideList strings.Builder
	for i := range deck.Slides {
		slideList.WriteString(fmt.Sprintf("  <p:sldId id=\"%d\" r:id=\"rId%d\"/>\n", 256+i, 10+i))
	}
	addFile(w, "ppt/presentation.xml", fmt.Sprintf(`<?xml version="1.0" encoding="UTF-8" standalone="yes"?>
<p:presentation xmlns:a="http://schemas.openxmlformats.org/drawingml/2006/main" xmlns:p="http://schemas.openxmlformats.org/presentationml/2006/main" xmlns:r="http://schemas.openxmlformats.org/officeDocument/2006/relationships">
  <p:sldMasterIdLst><p:sldMasterId id="2147483648" r:id="rId1"/></p:sldMasterIdLst>
  <p:sldIdLst>
%s  </p:sldIdLst>
  <p:sldSz cx="12192000" cy="6858000"/>
  <p:notesSz cx="6858000" cy="9144000"/>
</p:presentation>`, slideList.String()))

	var presRels strings.Builder
	presRels.WriteString(`<?xml version="1.0" encoding="UTF-8" standalone="yes"?>
<Relationships xmlns="http://schemas.openxmlformats.org/package/2006/relationships">
  <Relationship Id="rId1" Type="http://schemas.openxmlformats.org/officeDocument/2006/relationships/slideMaster" Target="slideMasters/slideMaster1.xml"/>`)
	for i := range deck.Slides {
		presRels.WriteString(fmt.Sprintf(`
  <Relationship Id="rId%d" Type="http://schemas.openxmlformats.org/officeDocument/2006/relationships/slide" Target="slides/slide%d.xml"/>`, 10+i, i+1))
	}
	presRels.WriteString("\n</Relationships>")
	addFile(w, "ppt/_rels/presentation.xml.rels", presRels.String())

	addFile(w, "ppt/slideMasters/slideMaster1.xml", `<?xml version="1.0" encoding="UTF-8" standalone="yes"?>
<p:sldMaster xmlns:a="http://schemas.openxmlformats.org/drawingml/2006/main" xmlns:p="http://schemas.openxmlformats.org/presentationml/2006/main" xmlns:r="http://schemas.openxmlformats.org/officeDocument/2006/relationships">
  <p:cSld><p:spTree><p:nvGrpSpPr><p:cNvPr id="1" name=""/><p:cNvGrpSpPr/><p:nvPr/></p:nvGrpSpPr><p:grpSpPr/></p:spTree></p:cSld>
  <p:clrMap bg1="lt1" tx1="dk1" bg2="lt2" tx2="dk2" accent1="accent1" accent2="accent2" accent3="accent3" accent4="accent4" accent5="accent5" accent6="accent6" hlink="hlink" folHlink="folHlink"/>
  <p:sldLayoutIdLst><p:sldLayoutId id="2147483649" r:id="rId1"/></p:sldLayoutIdLst>
</p:sldMaster>`)

	addFile(w, "ppt/slideMasters/_rels/slideMaster1.xml.rels", `<?xml version="1.0" encoding="UTF-8" standalone="yes"?>
<Relationships xmlns="http://schemas.openxmlformats.org/package/2006/relationships">
  <Relationship Id="rId1" Type="http://schemas.openxmlformats.org/officeDocument/2006/relationships/slideLayout" Target="../slideLayouts/slideLayout1.xml"/>
</Relationships>`)

	addFile(w, "ppt/slideLayouts/slideLayout1.xml", `<?xml version="1.0" encoding="UTF-8" standalone="yes"?>
<p:sldLayout xmlns:a="http://schemas.openxmlformats.org/drawingml/2006/main" xmlns:p="http://schemas.openxmlformats.org/presentationml/2006/main" xmlns:r="http://schemas.openxmlformats.org/officeDocument/2006/relationships" type="blank" preserve="1">
  <p:cSld name="Blank"><p:spTree><p:nvGrpSpPr><p:cNvPr id="1" name=""/><p:cNvGrpSpPr/><p:nvPr/></p:nvGrpSpPr><p:grpSpPr/></p:spTree></p:cSld>
</p:sldLayout>`)

	addFile(w, "ppt/slideLayouts/_rels/slideLayout1.xml.rels", `<?xml version="1.0" encoding="UTF-8" standalone="yes"?>
<Relationships xmlns="http://schemas.openxmlformats.org/package/2006/relationships">
  <Relationship Id="rId1" Type="http://schemas.openxmlformats.org/officeDocument/2006/relationships/slideMaster" Target="../slideMasters/slideMaster1.xml"/>
</Relationships>`)

	var notesBuf strings.Builder
	hasNotes := false
	for i, sl := range deck.Slides {
		slideNum := i + 1
		hasImage := false
		if sl.ImagePNG != "" {
			if data, err := os.ReadFile(sl.ImagePNG); err == nil {
				if fw, e := w.Create(fmt.Sprintf("ppt/media/image%d.png", slideNum)); e == nil {
					fw.Write(data)
					hasImage = true
				}
			}
		}
		addFile(w, fmt.Sprintf("ppt/slides/slide%d.xml", slideNum), buildEditableSlideXML(sl, hasImage))
		rels := `<?xml version="1.0" encoding="UTF-8" standalone="yes"?>
<Relationships xmlns="http://schemas.openxmlformats.org/package/2006/relationships">
  <Relationship Id="rId2" Type="http://schemas.openxmlformats.org/officeDocument/2006/relationships/slideLayout" Target="../slideLayouts/slideLayout1.xml"/>`
		if hasImage {
			rels += fmt.Sprintf(`
  <Relationship Id="rId1" Type="http://schemas.openxmlformats.org/officeDocument/2006/relationships/image" Target="../media/image%d.png"/>`, slideNum)
		}
		rels += "\n</Relationships>"
		addFile(w, fmt.Sprintf("ppt/slides/_rels/slide%d.xml.rels", slideNum), rels)

		if strings.TrimSpace(sl.Notes) != "" {
			hasNotes = true
			fmt.Fprintf(&notesBuf, "## Slide %d: %s\n\n%s\n\n", slideNum, sl.Title, sl.Notes)
		}
	}

	if hasNotes {
		_ = os.WriteFile(outPath+".notes.md", []byte("# 演讲备注\n\n"+notesBuf.String()), 0644)
	}
	return nil
}

// buildEditableSlideXML 生成单页 slide.xml: 可选背景图 + 标题文本框 + 正文要点文本框。
func buildEditableSlideXML(sl EditableSlide, hasImage bool) string {
	var b strings.Builder
	b.WriteString(`<?xml version="1.0" encoding="UTF-8" standalone="yes"?>
<p:sld xmlns:a="http://schemas.openxmlformats.org/drawingml/2006/main" xmlns:p="http://schemas.openxmlformats.org/presentationml/2006/main" xmlns:r="http://schemas.openxmlformats.org/officeDocument/2006/relationships">
  <p:cSld>
    <p:spTree>
      <p:nvGrpSpPr><p:cNvPr id="1" name=""/><p:cNvGrpSpPr/><p:nvPr/></p:nvGrpSpPr>
      <p:grpSpPr/>`)

	// 背景图 (铺满, 放最底层)
	if hasImage {
		b.WriteString(`
      <p:pic>
        <p:nvPicPr><p:cNvPr id="9" name="bg"/><p:cNvPicPr/><p:nvPr/></p:nvPicPr>
        <p:blipFill><a:blip r:embed="rId1"/><a:stretch><a:fillRect/></a:stretch></p:blipFill>
        <p:spPr><a:xfrm><a:off x="0" y="0"/><a:ext cx="12192000" cy="6858000"/></a:xfrm><a:prstGeom prst="rect"><a:avLst/></a:prstGeom></p:spPr>
      </p:pic>`)
	}

	// 标题
	title := sl.Title
	if sl.Subtitle != "" {
		title = title + "  —  " + sl.Subtitle
	}
	b.WriteString(textShape(2, "Title", 838200, 365125, 10515600, 1325563, []para{{text: title, sz: 3600, bold: true}}))

	// 正文要点
	if len(sl.Bullets) > 0 {
		paras := make([]para, 0, len(sl.Bullets))
		for _, bl := range sl.Bullets {
			paras = append(paras, para{text: bl, sz: 2000, bullet: true})
		}
		b.WriteString(textShape(3, "Content", 838200, 1825625, 10515600, 4351338, paras))
	}

	b.WriteString(`
    </p:spTree>
  </p:cSld>
</p:sld>`)
	return b.String()
}

type para struct {
	text   string
	sz     int
	bold   bool
	bullet bool
}

// textShape 生成一个含文本的 <p:sp> 形状 (EMU 坐标)。
func textShape(id int, name string, x, y, cx, cy int, paras []para) string {
	var body strings.Builder
	for _, p := range paras {
		buPr := "<a:buNone/>"
		if p.bullet {
			buPr = `<a:buChar char="•"/>`
		}
		boldAttr := ""
		if p.bold {
			boldAttr = ` b="1"`
		}
		fmt.Fprintf(&body, `<a:p><a:pPr>%s</a:pPr><a:r><a:rPr lang="zh-CN" sz="%d"%s dirty="0"/><a:t>%s</a:t></a:r></a:p>`,
			buPr, p.sz, boldAttr, escapeXML(p.text))
	}
	return fmt.Sprintf(`
      <p:sp>
        <p:nvSpPr><p:cNvPr id="%d" name="%s"/><p:cNvSpPr><a:spLocks noGrp="1"/></p:cNvSpPr><p:nvPr/></p:nvSpPr>
        <p:spPr><a:xfrm><a:off x="%d" y="%d"/><a:ext cx="%d" cy="%d"/></a:xfrm><a:prstGeom prst="rect"><a:avLst/></a:prstGeom></p:spPr>
        <p:txBody><a:bodyPr wrap="square" rtlCol="0"><a:normAutofit/></a:bodyPr><a:lstStyle/>%s</p:txBody>
      </p:sp>`, id, name, x, y, cx, cy, body.String())
}

// escapeXML 转义 XML 文本内容。
func escapeXML(s string) string {
	r := strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;", "\"", "&quot;", "'", "&apos;")
	return r.Replace(s)
}
