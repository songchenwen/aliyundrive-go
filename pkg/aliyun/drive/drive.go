// Package drive helps you to manage you aliyun drive files.
package drive

import (
	"archive/zip"
	"bytes"
	"context"
	"crypto/sha1"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"io/ioutil"
	"net/http"
	"net/url"
	"os"
	"path"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/pkg/errors"
)

const (
	FolderKind           = "folder"
	FileKind             = "file"
	AnyKind              = "any"
	DefaultPartSize      = 100 * 1024 * 1024 // 100 MiB. Will store this size of data in memory per file for upload
	DefaultUploadRetries = 10
)

const (
	apiRefreshToken        = "https://auth.aliyundrive.com/v2/account/token"
	apiPersonalInfo        = "https://api.aliyundrive.com/v2/databox/get_personal_info"
	apiList                = "https://api.aliyundrive.com/v2/file/list"
	apiCreate              = "https://api.aliyundrive.com/v2/file/create"
	apiUpdate              = "https://api.aliyundrive.com/v2/file/update"
	apiMove                = "https://api.aliyundrive.com/v2/file/move"
	apiCopy                = "https://api.aliyundrive.com/v2/file/copy"
	apiCreateFileWithProof = "https://api.aliyundrive.com/v2/file/create_with_proof"
	apiCompleteUpload      = "https://api.aliyundrive.com/v2/file/complete"
	apiGet                 = "https://api.aliyundrive.com/v2/file/get"
	apiGetByPath           = "https://api.aliyundrive.com/v2/file/get_by_path"
	apiCreateWithFolder    = "https://api.aliyundrive.com/adrive/v2/file/createWithFolders"
	apiTrash               = "https://api.aliyundrive.com/v2/recyclebin/trash"
	apiDelete              = "https://api.aliyundrive.com/v3/file/delete"
	apiBatch               = "https://api.aliyundrive.com/v2/batch"
	apiRefreshUpload       = "https://api.aliyundrive.com/v2/file/get_upload_url"

	fakeUA = "Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/91.0.4472.77 Safari/537.36"
)

var (
	ErrorLivpUpload      = errors.New("uploading .livp to album is not supported")
	ErrorTooManyRequests = errors.New("429 Too Many Requests")
	ErrorNotFound        = errors.New("404 Not Found")
	ErrorAlreadyExisted  = errors.New("already existed")
	ErrorMissingFields   = errors.New("required fields: ParentId, Name")
)

type Fs interface {
	About(ctx context.Context) (*PersonalSpaceInfo, error)
	Get(ctx context.Context, nodeId string) (*Node, error)
	GetByPath(ctx context.Context, fullPath string, kind string) (*Node, error)
	List(ctx context.Context, nodeId string) ([]Node, error)

	// CreateFolder creates a folder to aliyun drive.
	//
	// required Node fields: ParentId, Name.
	//
	// may return ErrorMissingFields if required fields are missing.
	CreateFolder(ctx context.Context, node Node) (nodeIdOut string, err error)
	Move(ctx context.Context, nodeId string, dstParentNodeId string, dstName string) (nodeIdOut string, err error)
	Remove(ctx context.Context, nodeId string) error
	Open(ctx context.Context, nodeId string, headers map[string]string) (io.ReadCloser, error)

	// CreateFile puts a file to aliyun drive.
	//
	// required Node fields: ParentId, Name.
	//
	// may return ErrorMissingFields if required fields are missing.
	CreateFile(ctx context.Context, node Node, in io.Reader) (nodeIdOut string, err error)
	CalcProof(fileSize int64, in *os.File) (file *os.File, proof string, err error)

	// CreateFileWithProof puts a file to aliyun drive.
	//
	// required Node fields: ParentId, Name.
	//
	// may return ErrorMissingFields if required fields are missing.
	CreateFileWithProof(ctx context.Context, node Node, in io.Reader, sha1Code string, proofCode string) (nodeIdOut string, err error)
	Copy(ctx context.Context, nodeId string, dstParentNodeId string, dstName string) (nodeIdOut string, err error)
	CreateFolderRecursively(ctx context.Context, fullPath string) (nodeIdOut string, err error)
	Update(ctx context.Context, node Node) (nodeIdOut string, err error)
}

type Config struct {
	RefreshToken    string
	IsAlbum         bool
	HttpClient      *http.Client
	UploadChunkSize int64
	UploadRetries   int
	OnRefreshToken  func(refreshToken string)
	LogFunc         func(format string, a ...interface{})
}

func (config Config) String() string {
	return fmt.Sprintf("Config{RefreshToken: %s}", config.RefreshToken)
}

type Drive struct {
	token
	config     Config
	driveId    string
	rootId     string
	rootNode   Node
	httpClient *http.Client
	mutex      sync.Mutex
}

type token struct {
	accessToken string
	expireAt    int64
}

func (drive *Drive) String() string {
	return fmt.Sprintf("AliyunDrive{driveId: %s}", drive.driveId)
}

func (drive *Drive) request(ctx context.Context, method, url string, headers map[string]string, body io.Reader) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, method, url, body)
	if err != nil {
		return nil, errors.WithStack(err)
	}

	req.Header.Set("Referer", "https://www.aliyundrive.com/")
	req.Header.Set("User-Agent", fakeUA)
	for k, v := range headers {
		req.Header.Set(k, v)
	}

	res, err2 := drive.httpClient.Do(req)
	if err2 != nil {
		return nil, errors.WithStack(err2)
	}

	return res, nil
}

func (drive *Drive) refreshToken(ctx context.Context) error {
	headers := map[string]string{
		"content-type": "application/json;charset=UTF-8",
	}
	data := map[string]string{
		"refresh_token": drive.config.RefreshToken,
		"grant_type":    "refresh_token",
	}

	var body io.Reader
	b, err := json.Marshal(&data)
	if err != nil {
		return errors.WithStack(err)
	}

	body = bytes.NewBuffer(b)
	res, err := drive.request(ctx, "POST", apiRefreshToken, headers, body)
	if err != nil {
		return errors.WithStack(err)
	}
	defer res.Body.Close()

	var token Token
	b, err = ioutil.ReadAll(res.Body)
	if err != nil {
		return errors.WithStack(err)
	}
	err = json.Unmarshal(b, &token)
	if err != nil {
		return errors.Wrapf(err, `failed to parse response "%s"`, string(b))
	}

	drive.accessToken = token.AccessToken
	drive.expireAt = token.ExpiresIn + time.Now().Unix()
	drive.config.RefreshToken = token.RefreshToken
	if drive.config.OnRefreshToken != nil {
		drive.config.OnRefreshToken(token.RefreshToken)
	}
	return nil
}

func (drive *Drive) jsonRequest(ctx context.Context, method, url string, request interface{}, response interface{}) error {
	// Token expired, refresh access
	if drive.expireAt < time.Now().Unix() {
		err := drive.refreshToken(ctx)
		if err != nil {
			return errors.WithStack(err)
		}
	}
	headers := map[string]string{
		"content-type":  "application/json;charset=UTF-8",
		"authorization": "Bearer " + drive.accessToken,
	}

	var bodyBytes []byte
	if request != nil {
		b, err := json.Marshal(request)
		if err != nil {
			return errors.WithStack(err)
		}
		bodyBytes = b
	}

	res, err := drive.request(ctx, method, url, headers, bytes.NewBuffer(bodyBytes))
	if err != nil {
		return errors.WithStack(err)
	}
	defer res.Body.Close()

	switch res.StatusCode {
	case http.StatusNotFound:
		return ErrorNotFound
	case http.StatusTooManyRequests:
		return ErrorTooManyRequests
	default:
	}

	if res.StatusCode >= 400 {
		return errors.Errorf(`failed to request "%s", got "%d"`, url, res.StatusCode)
	}

	if response != nil {
		b, err := ioutil.ReadAll(res.Body)
		if err != nil {
			return errors.WithStack(err)
		}
		err = json.Unmarshal(b, &response)
		if err != nil {
			return errors.Wrapf(err, `failed to parse response "%s"`, string(b))
		}
	}

	return nil
}

func NewFs(ctx context.Context, config *Config) (Fs, error) {
	if config.UploadChunkSize <= 0 {
		config.UploadChunkSize = DefaultPartSize
	}
	if config.UploadRetries <= 0 {
		config.UploadRetries = DefaultUploadRetries
	}
	if config.LogFunc == nil {
		config.LogFunc = func(format string, a ...interface{}) {}
	}
	drive := &Drive{
		config:     *config,
		httpClient: config.HttpClient,
	}

	// get driveId
	driveId := ""
	if config.IsAlbum {
		var albumInfo AlbumInfo
		data := map[string]string{}
		err := drive.jsonRequest(ctx, "POST", "https://api.aliyundrive.com/adrive/v1/user/albums_info", &data, &albumInfo)
		if err != nil {
			return nil, errors.Wrap(err, "failed to get driveId")
		}

		driveId = albumInfo.Data.DriveId
	} else {
		var user User
		data := map[string]string{}
		err := drive.jsonRequest(ctx, "POST", "https://api.aliyundrive.com/v2/user/get", &data, &user)
		if err != nil {
			return nil, errors.Wrap(err, "failed to get driveId")
		}

		driveId = user.DriveId
	}

	drive.driveId = driveId
	drive.rootId = "root"
	drive.rootNode = Node{
		NodeId: "root",
		Type:   "folder",
		Name:   "root",
	}

	return drive, nil
}

// path must start with "/" and must not end with "/"
func normalizePath(s string) string {
	separator := "/"
	if !strings.HasPrefix(s, separator) {
		s = separator + s
	}

	if len(s) > 1 && strings.HasSuffix(s, separator) {
		s = s[:len(s)-1]
	}
	return s
}

func (drive *Drive) listNodes(ctx context.Context, nodeId string) ([]Node, error) {
	data := map[string]interface{}{
		"drive_id":       drive.driveId,
		"parent_file_id": nodeId,
		"limit":          200,
		"marker":         "",
	}
	var nodes []Node
	var lNodes *ListNodes
	for {
		if lNodes != nil && lNodes.NextMarker == "" {
			break
		}

		err := drive.jsonRequest(ctx, "POST", apiList, &data, &lNodes)
		if err != nil {
			return nil, errors.WithStack(err)
		}

		nodes = append(nodes, lNodes.Items...)
		data["marker"] = lNodes.NextMarker
	}

	return nodes, nil
}

func (drive *Drive) findNameNode(ctx context.Context, nodeId string, name string, kind string) (*Node, error) {
	nodes, err := drive.listNodes(ctx, nodeId)
	if err != nil {
		return nil, errors.WithStack(err)
	}

	for _, d := range nodes {
		if d.Name == name && (kind == AnyKind || d.Type == kind) {
			return &d, nil
		}
	}

	return nil, errors.Wrapf(os.ErrNotExist, `can't find "%s", kind: "%s" under "%s"`, name, kind, nodeId)
}

// https://help.aliyun.com/document_detail/175927.html#h2-u83B7u53D6u6587u4EF6u6216u6587u4EF6u5939u4FE1u606F17
func (drive *Drive) Get(ctx context.Context, nodeId string) (*Node, error) {
	data := map[string]interface{}{
		"drive_id": drive.driveId,
		"file_id":  nodeId,
	}
	var node Node
	err := drive.jsonRequest(ctx, "POST", apiGet, &data, &node)
	if err != nil {
		return nil, err
	}

	return &node, nil
}

// https://help.aliyun.com/document_detail/175927.html#pdsgetfilebypathrequest
func (drive *Drive) GetByPath(ctx context.Context, fullPath string, kind string) (*Node, error) {
	fullPath = normalizePath(fullPath)

	if fullPath == "/" || fullPath == "" {
		return &drive.rootNode, nil
	}

	data := map[string]interface{}{
		"drive_id":  drive.driveId,
		"file_path": fullPath,
	}

	var node *Node
	err := drive.jsonRequest(ctx, "POST", apiGetByPath, &data, &node)
	// paths with surrounding white spaces (like `/ test / test1 `)
	// can't be found by `get_by_path`
	// https://github.com/K265/aliyundrive-go/issues/3
	if err != nil && !strings.Contains(err.Error(), `got "404"`) {
		return nil, errors.WithStack(err)
	}

	if node != nil && node.Type == kind {
		return node, nil
	}

	parent, name := path.Split(fullPath)
	parentNode, err := drive.GetByPath(ctx, parent, FolderKind)
	if err != nil {
		return nil, errors.Wrapf(err, `failed to request "%s"`, apiGetByPath)
	}

	return drive.findNameNode(ctx, parentNode.NodeId, name, kind)
}

func findNodeError(err error, path string) error {
	return errors.Wrapf(err, `failed to find node of "%s"`, path)
}

func (drive *Drive) About(ctx context.Context) (*PersonalSpaceInfo, error) {
	body := map[string]string{}
	var result PersonalInfo
	err := drive.jsonRequest(ctx, "POST", apiPersonalInfo, &body, &result)
	if err != nil {
		return nil, err
	}

	return &result.PersonalSpaceInfo, nil
}

func (drive *Drive) List(ctx context.Context, nodeId string) ([]Node, error) {
	return drive.listNodes(ctx, nodeId)
}

func createCheck(node Node) error {
	if node.ParentId == "" || node.Name == "" {
		return ErrorMissingFields
	}

	return nil
}

func (drive *Drive) CreateFolder(ctx context.Context, node Node) (string, error) {
	if err := createCheck(node); err != nil {
		return "", err
	}

	body := map[string]string{
		"drive_id":        drive.driveId,
		"check_name_mode": "refuse",
		"name":            node.Name,
		"parent_file_id":  node.ParentId,
		"type":            "folder",
		"meta":            node.Meta,
	}
	var result NodeId
	err := drive.jsonRequest(ctx, "POST", apiCreate, &body, &result)
	if err != nil {
		return "", errors.Wrap(err, "failed to post create folder request")
	}
	return result.NodeId, nil
}

func (drive *Drive) checkRoot(nodeId string) error {
	if nodeId == "" {
		return errors.New("empty nodeId")
	}
	if nodeId == drive.rootId {
		return errors.New("can't operate on root ")
	}
	return nil
}

func (drive *Drive) Move(ctx context.Context, nodeId string, dstParentNodeId string, dstName string) (string, error) {
	if err := drive.checkRoot(nodeId); err != nil {
		return "", err
	}

	body := map[string]string{
		"drive_id":          drive.driveId,
		"file_id":           nodeId,
		"to_parent_file_id": dstParentNodeId,
		"new_name":          dstName,
	}
	var result NodeId
	err := drive.jsonRequest(ctx, "POST", apiMove, &body, &result)
	if err != nil {
		return "", errors.Wrap(err, `failed to post move request`)
	}
	return result.NodeId, nil
}

func (drive *Drive) Remove(ctx context.Context, nodeId string) error {
	if err := drive.checkRoot(nodeId); err != nil {
		return err
	}

	body := map[string]string{
		"drive_id": drive.driveId,
		"file_id":  nodeId,
	}

	err := drive.jsonRequest(ctx, "POST", apiTrash, &body, nil)
	if err != nil {
		return errors.Wrap(err, `failed to post remove request`)
	}
	return nil
}

func (drive *Drive) getDownloadUrl(ctx context.Context, nodeId string) (*DownloadUrl, error) {
	var detail DownloadUrl
	data := map[string]string{
		"drive_id": drive.driveId,
		"file_id":  nodeId,
	}
	err := drive.jsonRequest(ctx, "POST", "https://api.aliyundrive.com/v2/file/get_download_url", &data, &detail)
	if err != nil {
		return nil, errors.Wrapf(err, `failed to get node detail of "%s"`, nodeId)
	}
	return &detail, nil
}

func (drive *Drive) Open(ctx context.Context, nodeId string, headers map[string]string) (io.ReadCloser, error) {
	if err := drive.checkRoot(nodeId); err != nil {
		return nil, err
	}

	downloadUrl, err := drive.getDownloadUrl(ctx, nodeId)
	if err != nil {
		return nil, err
	}

	url := downloadUrl.Url
	if url != "" {
		res, err := drive.request(ctx, "GET", url, headers, nil)
		if err != nil {
			return nil, errors.Wrapf(err, `failed to download "%s"`, url)
		}

		return res.Body, nil
	}

	// for iOS live photos (.livp)
	streamsUrl := downloadUrl.StreamsUrl
	if streamsUrl != nil {
		var buf bytes.Buffer
		zw := zip.NewWriter(&buf)
		for t, u := range streamsUrl {
			name := "output." + t
			w, err := zw.Create(name)
			if err != nil {
				return nil, errors.Wrapf(err, `failed to creat entry "%s" in zip file`, name)
			}

			res, err := drive.request(ctx, "GET", u, headers, nil)
			if err != nil {
				return nil, errors.Wrapf(err, `failed to download "%s"`, u)
			}

			if _, err := io.Copy(w, res.Body); err != nil {
				return nil, errors.Wrapf(err, `failed to write "%s" to zip`, name)
			}

			_ = res.Body.Close()
		}

		err := zw.Close()
		if err != nil {
			return nil, errors.WithStack(err)
		}

		return io.NopCloser(&buf), nil
	}

	return nil, errors.Errorf(`failed to open "%s"`, nodeId)
}

func CalcSha1(in *os.File) (*os.File, string, error) {
	h := sha1.New()
	_, err := io.Copy(h, in)
	if err != nil {
		return nil, "", errors.Wrap(err, "failed to calculate sha1")
	}

	_, _ = in.Seek(0, 0)
	return in, fmt.Sprintf("%X", h.Sum(nil)), nil
}

func calcProof(accessToken string, fileSize int64, in *os.File) (*os.File, string, error) {
	start := CalcProofOffset(accessToken, fileSize)
	sret, _ := in.Seek(start, 0)
	if sret != start {
		_, _ = in.Seek(0, 0)
		return in, "", errors.Errorf("failed to seek file to %d", start)
	}

	buf := make([]byte, 8)
	_, _ = in.Read(buf)
	proofCode := base64.StdEncoding.EncodeToString(buf)
	_, _ = in.Seek(0, 0)
	return in, proofCode, nil
}

func (drive *Drive) CalcProof(fileSize int64, in *os.File) (*os.File, string, error) {
	return calcProof(drive.accessToken, fileSize, in)
}

func (drive *Drive) CreateFile(ctx context.Context, node Node, in io.Reader) (string, error) {
	sha1Code := ""
	proofCode := ""

	fin, ok := in.(*os.File)
	if ok {
		in, sha1Code, _ = CalcSha1(fin)
		in, proofCode, _ = drive.CalcProof(node.Size, fin)
	}

	return drive.CreateFileWithProof(ctx, node, in, sha1Code, proofCode)
}

func makePartInfoList(size int64, partSize int64) []*PartInfo {
	partInfoNum := 0
	if size%partSize > 0 {
		partInfoNum++
	}
	partInfoNum += int(size / partSize)
	list := make([]*PartInfo, partInfoNum)
	for i := 0; i < partInfoNum; i++ {
		list[i] = &PartInfo{
			PartNumber: i + 1,
		}
	}
	return list
}

type CachedLimitReader struct {
	buf     []byte
	rd      io.Reader // reader provided by the client
	w, r    int
	length  int
	logFunc func(format string, a ...interface{})
	err     error
}

func NewCachedLimitReader(reader io.Reader, size int) *CachedLimitReader {
	return &CachedLimitReader{
		buf:    make([]byte, size),
		rd:     io.LimitReader(reader, int64(size)),
		w:      0,
		r:      0,
		length: size,
		logFunc: func(format string, a ...interface{}) {
			fmt.Printf(format, a...)
		},
	}
}

func NewCachedLimitReaderWithBuffer(reader io.Reader, buffer []byte, logFunc func(format string, a ...interface{})) *CachedLimitReader {
	if len(buffer) == 0 {
		panic("you can have an empty sized buffer")
	}
	size := len(buffer)
	return &CachedLimitReader{
		buf:     buffer,
		rd:      io.LimitReader(reader, int64(size)),
		w:       0,
		r:       0,
		length:  size,
		logFunc: logFunc,
	}
}

func min(a int, b int) int {
	if a > b {
		return b
	}
	return a
}

func minInt64(a int64, b int64) int64 {
	if a > b {
		return b
	}
	return a
}

func (r *CachedLimitReader) Read(p []byte) (n int, err error) {
	if len(p) == 0 {
		return
	}
	if r.err != nil {
		return 0, r.err
	}
	to_read := min(r.r+len(p), r.length) - r.w
	read_n := 0
	if to_read > 0 {
		read_n, err = r.rd.Read(r.buf[r.w : r.w+to_read])
	}
	r.w += read_n
	if err != nil {
		r.logFunc("read err read_n:%d r:%d w:%d %v\n", read_n, r.r, r.w, err)
	}
	if to_read > 0 && err == io.EOF {
		if r.length > r.w {
			r.length = r.w
		}
	}
	n = min(r.w-r.r, len(p))
	if n == 0 {
		if r.length == r.r {
			err = io.EOF
		} else {
			r.logFunc("read unexpected no data r:%d w:%d, %v\n", r.r, r.w, err)
		}
		return
	}
	copy(p, r.buf[r.r:r.r+n])
	r.r = r.r + n
	if err != nil && err != io.EOF {
		r.logFunc("read last err %v\n", err)
		r.err = err
	}
	return
}

func (drive *Drive) refreshUploadUrl(ctx context.Context, uploadID string, fileID string, outParts []PartInfo) error {
	partList := make([]*PartInfo, len(outParts))
	for i := 0; i < len(outParts); i++ {
		partList[i] = &PartInfo{
			PartNumber: i + 1,
		}
	}
	uploadInfo := UploadInfo{
		DriveID:      drive.driveId,
		PartInfoList: partList,
		FileId:       fileID,
		UploadId:     uploadID,
	}
	var uploadResult UploadResult
	err := drive.jsonRequest(ctx, "POST", apiRefreshUpload, uploadInfo, &uploadResult)
	if err != nil {
		return err
	}
	if len(uploadResult.PartInfoList) != len(outParts) {
		return errors.Errorf("refresh upload url invalid result length %d -> %d", len(outParts), len(uploadResult.PartInfoList))
	}
	for i, part := range uploadResult.PartInfoList {
		outParts[i].UploadUrl = part.UploadUrl
	}
	return nil
}

func isUploadUrlValid(uploadUrl string) bool {
	u, err := url.Parse(uploadUrl)
	if err != nil {
		return false
	}
	expireStr := u.Query().Get("x-oss-expires")
	if len(expireStr) == 0 {
		return true
	}
	expire, err := strconv.Atoi(expireStr)
	if err != nil {
		return false
	}
	timeStep := 60 * 10
	return int(time.Now().Unix()) < expire-timeStep
}

func (drive *Drive) CreateFileWithProof(ctx context.Context, node Node, in io.Reader, sha1Code string, proofCode string) (string, error) {
	if err := createCheck(node); err != nil {
		return "", err
	}

	if strings.HasSuffix(strings.ToLower(node.Name), ".livp") {
		return "", ErrorLivpUpload
	}

	var proofResult ProofResult

	proof := &FileProof{
		DriveID:         drive.driveId,
		PartInfoList:    makePartInfoList(node.Size, drive.config.UploadChunkSize),
		ParentFileID:    node.ParentId,
		Name:            node.Name,
		Type:            "file",
		CheckNameMode:   "refuse",
		Size:            node.Size,
		ContentHash:     sha1Code,
		ContentHashName: "sha1",
		ProofCode:       proofCode,
		ProofVersion:    "v1",
		Meta:            node.Meta,
	}

	{
		err := drive.jsonRequest(ctx, "POST", apiCreateFileWithProof, proof, &proofResult)
		if err != nil {
			return "", errors.Wrap(err, `failed to post create file request`)
		}

		if proofResult.RapidUpload {
			return proofResult.FileId, nil
		}

		if proofResult.Exist {
			return "", ErrorAlreadyExisted
		}

		if len(proofResult.PartInfoList) < 1 {
			return "", errors.New(`failed to extract uploadUrl`)
		}
	}
	uploadBuffer := make([]byte, minInt64(drive.config.UploadChunkSize, node.Size))
	uploadedSize := 0
	drive.config.LogFunc("begin to upload %v, with ctx %v\n", node.Name, ctx)
	for _, part := range proofResult.PartInfoList {
		partSize := drive.config.UploadChunkSize
		if part.PartNumber == len(proofResult.PartInfoList) {
			partSize = node.Size % drive.config.UploadChunkSize
		}
		partReader := NewCachedLimitReaderWithBuffer(in, uploadBuffer, func(format string, a ...interface{}) {
			f := fmt.Sprintf("%s %s", node.Name, format)
			drive.config.LogFunc(f, a...)
		})
		tries := 0
		for {
			tries++
			if partReader.err != nil {
				return "", partReader.err
			}
			if !isUploadUrlValid(part.UploadUrl) {
				drive.config.LogFunc("refreshing upload url for %d/%d %v, ", part.PartNumber, len(proofResult.PartInfoList), node.Name)
				err := drive.refreshUploadUrl(ctx, proofResult.UploadId, proofResult.FileId, proofResult.PartInfoList)
				if err != nil {
					if tries < drive.config.UploadRetries && ctx.Err() == nil {
						drive.config.LogFunc("refresh upload url failed %v, retry %d/%d, %v\n", part.UploadUrl, tries, drive.config.UploadRetries, err)
						continue
					} else {
						return "", errors.Wrap(err, fmt.Sprintf("refresh upload url failed %v", err))
					}
				}
				part = proofResult.PartInfoList[part.PartNumber-1]
			}
			partReader.r = 0
			req, err := http.NewRequestWithContext(ctx, "PUT", part.UploadUrl, partReader)
			if err != nil {
				return "", errors.Wrap(err, "failed to create upload request")
			}
			resp, err := drive.httpClient.Do(req)
			if err != nil {
				if tries < drive.config.UploadRetries && ctx.Err() == nil {
					drive.config.LogFunc("failed to upload file part %d/%d of %v, end at %d/%d/%d/%d, retries %d/%d, %v\n", part.PartNumber, len(proofResult.PartInfoList), node.Name, partReader.r, drive.config.UploadChunkSize, uploadedSize+partReader.r, node.Size, tries, drive.config.UploadRetries, err)
					continue
				} else {
					return "", errors.Wrap(err, "failed to upload file")
				}
			}
			resp.Body.Close()
			if partReader.r < int(partSize) {
				drive.config.LogFunc("upload part %d/%d of %v, not complete %d %d, ctx err %v\n", part.PartNumber, len(proofResult.PartInfoList), node.Name, partReader.r, partSize, ctx.Err())
				return "", errors.Wrap(err, "failed to upload file read not complete")
			}
			uploadedSize += partReader.r
			drive.config.LogFunc("uploaded %d/%d of %v tries %d/%d size %d %d/%d %d percent\n", part.PartNumber, len(proofResult.PartInfoList), node.Name, tries, drive.config.UploadRetries, partReader.r, uploadedSize, node.Size, uploadedSize*100/int(node.Size))
			break
		}
	}

	var result NodeId
	{
		body := map[string]interface{}{
			"drive_id":  drive.driveId,
			"file_id":   proofResult.FileId,
			"upload_id": proofResult.UploadId,
		}

		err := drive.jsonRequest(ctx, "POST", apiCompleteUpload, &body, &result)
		if err != nil {
			return "", errors.Wrap(err, `failed to post upload complete request`)
		}
	}
	return result.NodeId, nil
}

// https://help.aliyun.com/document_detail/175927.html#pdscopyfilerequest
func (drive *Drive) Copy(ctx context.Context, nodeId string, dstParentNodeId string, dstName string) (string, error) {
	body := map[string]string{
		"drive_id":          drive.driveId,
		"file_id":           nodeId,
		"to_parent_file_id": dstParentNodeId,
		"new_name":          dstName,
	}
	var result NodeId
	err := drive.jsonRequest(ctx, "POST", apiCopy, &body, &result)
	if err != nil {
		return "", errors.Wrap(err, `failed to post copy request`)
	}

	return result.NodeId, nil
}

func (drive *Drive) createFolderInternal(ctx context.Context, parent string, name string) (string, error) {
	drive.mutex.Lock()
	defer drive.mutex.Unlock()

	node, err := drive.GetByPath(ctx, parent+"/"+name, FolderKind)
	if err == nil {
		return node.NodeId, nil
	}

	node, err = drive.GetByPath(ctx, parent, FolderKind)
	if err != nil {
		return "", findNodeError(err, parent)
	}
	body := map[string]string{
		"drive_id":        drive.driveId,
		"check_name_mode": "refuse",
		"name":            name,
		"parent_file_id":  node.NodeId,
		"type":            "folder",
	}
	var result NodeId
	err = drive.jsonRequest(ctx, "POST", apiCreate, &body, &result)
	if err != nil {
		return "", errors.Wrap(err, "failed to post create folder request")
	}
	return result.NodeId, nil
}

func (drive *Drive) CreateFolderRecursively(ctx context.Context, fullPath string) (string, error) {
	fullPath = normalizePath(fullPath)
	pathLen := len(fullPath)
	i := 0
	var nodeId string
	for i < pathLen {
		parent := fullPath[:i]
		remain := fullPath[i+1:]
		j := strings.Index(remain, "/")
		if j < 0 {
			// already at last position
			return drive.createFolderInternal(ctx, parent, remain)
		} else {
			nodeId2, err := drive.createFolderInternal(ctx, parent, remain[:j])
			if err != nil {
				return "", err
			}
			i = i + j + 1
			nodeId = nodeId2
		}
	}

	return nodeId, nil
}

func (drive *Drive) Update(ctx context.Context, node Node) (string, error) {
	body := map[string]string{
		"drive_id": drive.driveId,
		"file_id":  node.NodeId,
		//"check_name_mode": "refuse",
		"name": node.Name,
		"meta": node.Meta,
	}
	var result NodeId
	err := drive.jsonRequest(ctx, "POST", apiUpdate, &body, &result)
	if err != nil {
		return "", errors.Wrap(err, "failed to post update request")
	}
	return result.NodeId, nil
}
