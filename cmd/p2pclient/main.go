package main

import (
	"archive/zip"
	"bufio"
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"
)

type apiResp struct {
	Code    int             `json:"code"`
	Message string          `json:"message"`
	Data    json.RawMessage `json:"data"`
}

type createShareResp struct {
	Token string `json:"token"`
	Name  string `json:"name"`
}

type offerData struct {
	ListenAddr string `json:"listen_addr"`
	FileName   string `json:"file_name"`
	FileSize   int64  `json:"file_size_bytes"`
}

type offerQueryResp struct {
	Share map[string]interface{} `json:"share"`
	Offer offerData              `json:"offer"`
}

type fileHeader struct {
	Name   string `json:"name"`
	Size   int64  `json:"size"`
	SHA256 string `json:"sha256"`
}

func main() {
	if len(os.Args) < 2 {
		printUsage()
		os.Exit(2)
	}

	sub := os.Args[1]
	switch sub {
	case "host":
		if err := runHost(os.Args[2:]); err != nil {
			fmt.Fprintf(os.Stderr, "[ERROR] host failed: %v\n", err)
			os.Exit(1)
		}
	case "recv":
		if err := runRecv(os.Args[2:]); err != nil {
			fmt.Fprintf(os.Stderr, "[ERROR] recv failed: %v\n", err)
			os.Exit(1)
		}
	default:
		printUsage()
		os.Exit(2)
	}
}

func printUsage() {
	fmt.Println("Usage:")
	fmt.Println("  p2pclient host --server http://127.0.0.1:8080 --auth-token <token> --node-type <file|folder> --node-id <id> --listen :9099 --advertise 192.168.1.10:9099 [--source /path/file] [--source-dir /path/dir]")
	fmt.Println("  p2pclient recv --server http://127.0.0.1:8080 --share-token <shareToken> [--password xxx] [--output-dir ./downloads]")
}

func runHost(args []string) error {
	fs := flag.NewFlagSet("host", flag.ContinueOnError)
	server := fs.String("server", "http://127.0.0.1:8080", "server base url")
	authToken := fs.String("auth-token", "", "bearer token")
	nodeType := fs.String("node-type", "file", "netdisk node type: file or folder")
	nodeID := fs.String("node-id", "", "netdisk node id")
	fileID := fs.String("file-id", "", "legacy alias of --node-id")
	listenAddr := fs.String("listen", ":9099", "local listen addr")
	advertiseAddr := fs.String("advertise", "", "address announced to peer, e.g. 192.168.1.10:9099")
	sourcePath := fs.String("source", "", "local file source path")
	sourceDir := fs.String("source-dir", "", "directory to be zipped and sent")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if strings.TrimSpace(*nodeID) == "" {
		*nodeID = strings.TrimSpace(*fileID)
	}
	*nodeType = strings.ToLower(strings.TrimSpace(*nodeType))
	if *nodeType != "file" && *nodeType != "folder" {
		return errors.New("node-type must be file or folder")
	}
	if strings.TrimSpace(*authToken) == "" || strings.TrimSpace(*nodeID) == "" || strings.TrimSpace(*advertiseAddr) == "" {
		return errors.New("auth-token, node-id, advertise are required")
	}
	if strings.TrimSpace(*sourcePath) == "" && strings.TrimSpace(*sourceDir) == "" {
		return errors.New("source or source-dir is required")
	}
	if strings.TrimSpace(*sourcePath) != "" && strings.TrimSpace(*sourceDir) != "" {
		return errors.New("source and source-dir are mutually exclusive")
	}

	share, err := createP2PShare(*server, *authToken, *nodeType, *nodeID)
	if err != nil {
		return err
	}

	path := strings.TrimSpace(*sourcePath)
	cleanup := func() {}
	if strings.TrimSpace(*sourceDir) != "" {
		tmpZip, err := zipDirectory(strings.TrimSpace(*sourceDir))
		if err != nil {
			return err
		}
		path = tmpZip
		cleanup = func() { _ = os.Remove(tmpZip) }
	}
	defer cleanup()

	info, err := os.Stat(path)
	if err != nil {
		return err
	}
	sum, err := fileSHA256(path)
	if err != nil {
		return err
	}
	fileName := defaultIfEmpty(share.Name, filepath.Base(path))
	// 新增：如果是通过 --source-dir 打包的 ZIP，确保文件名以 .zip 结尾
	if strings.TrimSpace(*sourceDir) != "" && !strings.HasSuffix(strings.ToLower(fileName), ".zip") {
		fileName += ".zip"
	}
	offerPayload := map[string]interface{}{
		"listen_addr":     strings.TrimSpace(*advertiseAddr),
		"file_name":       fileName,
		"file_size_bytes": info.Size(),
		"file_id":         strings.TrimSpace(*nodeID),
		"node_type":       *nodeType,
	}
	if err := postOffer(*server, *authToken, share.Token, offerPayload); err != nil {
		return err
	}

	fmt.Printf("[INFO] P2P share token: %s\n", share.Token)
	fmt.Printf("[INFO] Offer published: %s\n", strings.TrimSpace(*advertiseAddr))

	ln, err := net.Listen("tcp", strings.TrimSpace(*listenAddr))
	if err != nil {
		return err
	}
	defer ln.Close()
	fmt.Printf("[INFO] Waiting peer at %s ...\n", ln.Addr().String())

	for {
		conn, err := ln.Accept()
		if err != nil {
			return err
		}
		go func(c net.Conn) {
			defer c.Close()
			h := fileHeader{Name: fileName, Size: info.Size(), SHA256: sum}
			headBytes, _ := json.Marshal(h)
			if _, err := c.Write(append(headBytes, '\n')); err != nil {
				return
			}
			f, err := os.Open(path)
			if err != nil {
				return
			}
			defer f.Close()
			_, _ = io.Copy(c, f)
		}(conn)
	}
}

func runRecv(args []string) error {
	fs := flag.NewFlagSet("recv", flag.ContinueOnError)
	server := fs.String("server", "http://127.0.0.1:8080", "server base url")
	shareToken := fs.String("share-token", "", "p2p share token")
	password := fs.String("password", "", "optional share password")
	outputDir := fs.String("output-dir", ".", "output directory")
	receiverAddr := fs.String("receiver-addr", "", "optional local receiver addr for answer")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if strings.TrimSpace(*shareToken) == "" {
		return errors.New("share-token is required")
	}

	offerResp, err := getOffer(*server, strings.TrimSpace(*shareToken), strings.TrimSpace(*password))
	if err != nil {
		return err
	}
	if strings.TrimSpace(*receiverAddr) != "" {
		_ = postAnswer(*server, strings.TrimSpace(*shareToken), strings.TrimSpace(*password), strings.TrimSpace(*receiverAddr))
	}

	addr := strings.TrimSpace(offerResp.Offer.ListenAddr)
	if addr == "" {
		return errors.New("offer listen_addr is empty")
	}

	conn, err := net.DialTimeout("tcp", addr, 8*time.Second)
	if err != nil {
		return err
	}
	defer conn.Close()

	reader := bufio.NewReader(conn)
	headerLine, err := reader.ReadBytes('\n')
	if err != nil {
		return err
	}
	var h fileHeader
	if err := json.Unmarshal(bytes.TrimSpace(headerLine), &h); err != nil {
		return err
	}
	if h.Name == "" {
		h.Name = "p2p_received.bin"
	}
	if err := os.MkdirAll(*outputDir, 0o755); err != nil {
		return err
	}
	outPath := filepath.Join(*outputDir, h.Name)
	out, err := os.Create(outPath)
	if err != nil {
		return err
	}
	defer out.Close()

	hasher := sha256.New()
	w := io.MultiWriter(out, hasher)
	if _, err := io.Copy(w, reader); err != nil {
		return err
	}
	got := hex.EncodeToString(hasher.Sum(nil))
	if h.SHA256 != "" && !strings.EqualFold(got, h.SHA256) {
		return fmt.Errorf("sha256 mismatch, got=%s want=%s", got, h.SHA256)
	}
	fmt.Printf("[OK] received file: %s\n", outPath)
	fmt.Printf("[OK] sha256: %s\n", got)
	return nil
}

func createP2PShare(server, authToken, nodeType, nodeID string) (*createShareResp, error) {
	payload := map[string]interface{}{
		"share_type": "p2p_file",
		"node_type":  nodeType,
		"node_id":    nodeID,
	}
	b, _ := json.Marshal(payload)
	req, err := http.NewRequest(http.MethodPost, strings.TrimRight(server, "/")+"/api/v1/shares", bytes.NewReader(b))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+authToken)

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	var a apiResp
	if err := json.NewDecoder(resp.Body).Decode(&a); err != nil {
		return nil, err
	}
	if a.Code != 0 {
		return nil, fmt.Errorf("create share failed: %s", a.Message)
	}
	var out createShareResp
	if err := json.Unmarshal(a.Data, &out); err != nil {
		return nil, err
	}
	if out.Token == "" {
		return nil, errors.New("empty share token")
	}
	return &out, nil
}

func postOffer(server, authToken, shareToken string, offer map[string]interface{}) error {
	b, _ := json.Marshal(offer)
	url := strings.TrimRight(server, "/") + "/api/v1/p2p/signals/" + shareToken + "/offer"
	req, err := http.NewRequest(http.MethodPost, url, bytes.NewReader(b))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+authToken)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	var a apiResp
	if err := json.NewDecoder(resp.Body).Decode(&a); err != nil {
		return err
	}
	if a.Code != 0 {
		return fmt.Errorf("publish offer failed: %s", a.Message)
	}
	return nil
}

func getOffer(server, shareToken, password string) (*offerQueryResp, error) {
	base := strings.TrimRight(server, "/") + "/api/v1/p2p/signals/" + shareToken + "/offer"
	if password != "" {
		base += "?password=" + url.QueryEscape(password)
	}
	resp, err := http.Get(base)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	var a apiResp
	if err := json.NewDecoder(resp.Body).Decode(&a); err != nil {
		return nil, err
	}
	if a.Code != 0 {
		return nil, fmt.Errorf("get offer failed: %s", a.Message)
	}
	var out offerQueryResp
	if err := json.Unmarshal(a.Data, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

func postAnswer(server, shareToken, password, receiverAddr string) error {
	payload := map[string]string{"receiver_addr": receiverAddr}
	b, _ := json.Marshal(payload)
	u := strings.TrimRight(server, "/") + "/api/v1/p2p/signals/" + shareToken + "/answer"
	if password != "" {
		u += "?password=" + url.QueryEscape(password)
	}
	req, err := http.NewRequest(http.MethodPost, u, bytes.NewReader(b))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	var a apiResp
	if err := json.NewDecoder(resp.Body).Decode(&a); err != nil {
		return err
	}
	if a.Code != 0 {
		return fmt.Errorf("publish answer failed: %s", a.Message)
	}
	return nil
}

func fileSHA256(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

func defaultIfEmpty(v, d string) string {
	if strings.TrimSpace(v) == "" {
		return d
	}
	return v
}

func zipDirectory(dir string) (string, error) {
	baseInfo, err := os.Stat(dir)
	if err != nil {
		return "", err
	}
	if !baseInfo.IsDir() {
		return "", errors.New("source-dir must be a directory")
	}

	archive, err := os.CreateTemp("", "p2p-folder-*.zip")
	if err != nil {
		return "", err
	}
	defer archive.Close()

	zipWriter := zip.NewWriter(archive)
	rootName := filepath.Base(dir)

	err = filepath.Walk(dir, func(path string, info os.FileInfo, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		rel, err := filepath.Rel(dir, path)
		if err != nil {
			return err
		}
		if rel == "." {
			return nil
		}

		zipPath := filepath.ToSlash(filepath.Join(rootName, rel))
		if info.IsDir() {
			_, err := zipWriter.Create(zipPath + "/")
			return err
		}

		h, err := zip.FileInfoHeader(info)
		if err != nil {
			return err
		}
		h.Name = zipPath
		h.Method = zip.Deflate
		w, err := zipWriter.CreateHeader(h)
		if err != nil {
			return err
		}

		f, err := os.Open(path)
		if err != nil {
			return err
		}
		defer f.Close()
		_, err = io.Copy(w, f)
		return err
	})
	if err != nil {
		_ = zipWriter.Close()
		_ = os.Remove(archive.Name())
		return "", err
	}

	if err := zipWriter.Close(); err != nil {
		_ = os.Remove(archive.Name())
		return "", err
	}
	return archive.Name(), nil
}
