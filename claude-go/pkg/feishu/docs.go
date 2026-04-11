// Package feishu — 飞书文档/表格/多维表格生态操作封装。
//
// 通过飞书开放平台 API 操作:
//   - 文档 (Docx): 创建、读取、更新
//   - 电子表格 (Sheets): 创建、读写单元格
//   - 多维表格 (Bitable): 创建、CRUD 记录
//   - 云空间 (Drive): 文件上传/下载
//   - 知识库 (Wiki): 创建/查询知识空间
package feishu

import (
	"context"
	"fmt"

	lark "github.com/larksuite/oapi-sdk-go/v3"
	larkbitable "github.com/larksuite/oapi-sdk-go/v3/service/bitable/v1"
	larkdocx "github.com/larksuite/oapi-sdk-go/v3/service/docx/v1"
	larkdrive "github.com/larksuite/oapi-sdk-go/v3/service/drive/v1"
	larksheets "github.com/larksuite/oapi-sdk-go/v3/service/sheets/v3"
	larkwiki "github.com/larksuite/oapi-sdk-go/v3/service/wiki/v2"
)

// DocsClient 飞书文档生态操作客户端。
type DocsClient struct {
	client *lark.Client
}

// NewDocsClient 创建文档操作客户端。
func NewDocsClient(client *lark.Client) *DocsClient {
	return &DocsClient{client: client}
}

// --- 文档 (Docx) ---

// CreateDocument 创建飞书文档并返回 document_id。
func (d *DocsClient) CreateDocument(ctx context.Context, title string, folderToken string) (string, error) {
	body := larkdocx.NewCreateDocumentReqBodyBuilder().Title(title)
	if folderToken != "" {
		body = body.FolderToken(folderToken)
	}
	req := larkdocx.NewCreateDocumentReqBuilder().Body(body.Build()).Build()

	resp, err := d.client.Docx.Document.Create(ctx, req)
	if err != nil {
		return "", fmt.Errorf("创建文档失败: %w", err)
	}
	if !resp.Success() {
		return "", fmt.Errorf("创建文档失败: code=%d, msg=%s", resp.Code, resp.Msg)
	}
	if resp.Data == nil || resp.Data.Document == nil {
		return "", fmt.Errorf("创建文档返回空数据")
	}
	return deref(resp.Data.Document.DocumentId), nil
}

// GetDocumentContent 获取文档纯文本内容。
func (d *DocsClient) GetDocumentContent(ctx context.Context, documentID string) (string, error) {
	req := larkdocx.NewRawContentDocumentReqBuilder().DocumentId(documentID).Build()
	resp, err := d.client.Docx.Document.RawContent(ctx, req)
	if err != nil {
		return "", fmt.Errorf("获取文档内容失败: %w", err)
	}
	if !resp.Success() {
		return "", fmt.Errorf("获取文档内容失败: code=%d, msg=%s", resp.Code, resp.Msg)
	}
	if resp.Data == nil {
		return "", nil
	}
	return deref(resp.Data.Content), nil
}

// --- 电子表格 (Sheets) ---

// CreateSpreadsheet 创建电子表格并返回 spreadsheet_token。
func (d *DocsClient) CreateSpreadsheet(ctx context.Context, title, folderToken string) (string, error) {
	ssBuilder := larksheets.NewSpreadsheetBuilder().Title(title)
	if folderToken != "" {
		ssBuilder = ssBuilder.FolderToken(folderToken)
	}
	req := larksheets.NewCreateSpreadsheetReqBuilder().
		Spreadsheet(ssBuilder.Build()).
		Build()

	resp, err := d.client.Sheets.Spreadsheet.Create(ctx, req)
	if err != nil {
		return "", fmt.Errorf("创建表格失败: %w", err)
	}
	if !resp.Success() {
		return "", fmt.Errorf("创建表格失败: code=%d, msg=%s", resp.Code, resp.Msg)
	}
	if resp.Data == nil || resp.Data.Spreadsheet == nil {
		return "", fmt.Errorf("创建表格返回空数据")
	}
	return deref(resp.Data.Spreadsheet.SpreadsheetToken), nil
}

// GetSpreadsheetInfo 获取表格元信息。
func (d *DocsClient) GetSpreadsheetInfo(ctx context.Context, token string) (string, error) {
	req := larksheets.NewGetSpreadsheetReqBuilder().SpreadsheetToken(token).Build()
	resp, err := d.client.Sheets.Spreadsheet.Get(ctx, req)
	if err != nil {
		return "", fmt.Errorf("获取表格信息失败: %w", err)
	}
	if !resp.Success() {
		return "", fmt.Errorf("获取表格信息失败: code=%d, msg=%s", resp.Code, resp.Msg)
	}
	if resp.Data == nil || resp.Data.Spreadsheet == nil {
		return "", nil
	}
	return deref(resp.Data.Spreadsheet.Title), nil
}

// --- 多维表格 (Bitable) ---

// CreateBitable 创建多维表格。
func (d *DocsClient) CreateBitable(ctx context.Context, name, folderToken string) (string, error) {
	appBuilder := larkbitable.NewReqAppBuilder().Name(name)
	if folderToken != "" {
		appBuilder = appBuilder.FolderToken(folderToken)
	}
	req := larkbitable.NewCreateAppReqBuilder().
		ReqApp(appBuilder.Build()).
		Build()

	resp, err := d.client.Bitable.App.Create(ctx, req)
	if err != nil {
		return "", fmt.Errorf("创建多维表格失败: %w", err)
	}
	if !resp.Success() {
		return "", fmt.Errorf("创建多维表格失败: code=%d, msg=%s", resp.Code, resp.Msg)
	}
	if resp.Data == nil || resp.Data.App == nil {
		return "", fmt.Errorf("创建多维表格返回空数据")
	}
	return deref(resp.Data.App.AppToken), nil
}

// ListBitableRecords 查询多维表格记录。
func (d *DocsClient) ListBitableRecords(ctx context.Context, appToken, tableID string) ([]*larkbitable.AppTableRecord, error) {
	req := larkbitable.NewListAppTableRecordReqBuilder().
		AppToken(appToken).
		TableId(tableID).
		Build()

	resp, err := d.client.Bitable.AppTableRecord.List(ctx, req)
	if err != nil {
		return nil, fmt.Errorf("查询多维表格记录失败: %w", err)
	}
	if !resp.Success() {
		return nil, fmt.Errorf("查询多维表格记录失败: code=%d, msg=%s", resp.Code, resp.Msg)
	}
	if resp.Data == nil {
		return nil, nil
	}
	return resp.Data.Items, nil
}

// --- 云空间 (Drive) ---

// ListDriveFiles 列出云空间文件夹内容。
func (d *DocsClient) ListDriveFiles(ctx context.Context, folderToken string) ([]string, error) {
	req := larkdrive.NewListFileReqBuilder().FolderToken(folderToken).Build()
	resp, err := d.client.Drive.File.List(ctx, req)
	if err != nil {
		return nil, fmt.Errorf("列出文件失败: %w", err)
	}
	if !resp.Success() {
		return nil, fmt.Errorf("列出文件失败: code=%d, msg=%s", resp.Code, resp.Msg)
	}
	if resp.Data == nil {
		return nil, nil
	}
	var names []string
	for _, f := range resp.Data.Files {
		names = append(names, deref(f.Name))
	}
	return names, nil
}

// --- 知识库 (Wiki) ---

// ListWikiSpaces 列出知识库空间。
func (d *DocsClient) ListWikiSpaces(ctx context.Context) ([]string, error) {
	req := larkwiki.NewListSpaceReqBuilder().Build()
	resp, err := d.client.Wiki.Space.List(ctx, req)
	if err != nil {
		return nil, fmt.Errorf("列出知识库失败: %w", err)
	}
	if !resp.Success() {
		return nil, fmt.Errorf("列出知识库失败: code=%d, msg=%s", resp.Code, resp.Msg)
	}
	if resp.Data == nil {
		return nil, nil
	}
	var result []string
	for _, s := range resp.Data.Items {
		result = append(result, deref(s.Name))
	}
	return result, nil
}

// GetWikiNode 获取知识库节点内容。
func (d *DocsClient) GetWikiNode(ctx context.Context, spaceID, nodeToken string) (string, string, error) {
	req := larkwiki.NewGetNodeSpaceReqBuilder().Token(nodeToken).Build()
	resp, err := d.client.Wiki.Space.GetNode(ctx, req)
	if err != nil {
		return "", "", fmt.Errorf("获取知识节点失败: %w", err)
	}
	if !resp.Success() {
		return "", "", fmt.Errorf("获取知识节点失败: code=%d, msg=%s", resp.Code, resp.Msg)
	}
	if resp.Data == nil || resp.Data.Node == nil {
		return "", "", nil
	}
	return deref(resp.Data.Node.Title), deref(resp.Data.Node.ObjToken), nil
}
