package api

import (
	"bufio"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestTCPStalledExpectContinueUploadDoesNotBlockList(t *testing.T) {
	h, _ := apiTestHandler(t, nil)
	server := httptest.NewUnstartedServer(h)
	server.Config.ReadTimeout = 30 * time.Second
	server.Start()

	var stalled net.Conn
	t.Cleanup(func() {
		// Closing the incomplete upload first lets its handler finish immediately,
		// including after a test failure, rather than waiting for ReadTimeout.
		if stalled != nil {
			_ = stalled.Close()
		}
		server.Close()
	})
	var err error
	stalled, err = net.DialTimeout("tcp", server.Listener.Addr().String(), 2*time.Second)
	if err != nil {
		t.Fatalf("connect stalled upload: %v", err)
	}
	if err := stalled.SetDeadline(time.Now().Add(2 * time.Second)); err != nil {
		t.Fatalf("set upload handshake deadline: %v", err)
	}
	_, err = fmt.Fprintf(stalled, "POST /v1/records HTTP/1.1\r\nHost: %s\r\nAuthorization: Bearer %s\r\nContent-Type: application/json\r\nContent-Length: 100\r\nExpect: 100-continue\r\n\r\n", server.Listener.Addr().String(), apiTestToken)
	if err != nil {
		t.Fatalf("send upload headers: %v", err)
	}
	interim, err := http.ReadResponse(bufio.NewReader(stalled), &http.Request{Method: http.MethodPost})
	if err != nil {
		t.Fatalf("read 100 Continue response: %v", err)
	}
	_ = interim.Body.Close()
	if interim.StatusCode != http.StatusContinue {
		t.Fatalf("upload response = %d, want 100 Continue", interim.StatusCode)
	}
	// net/http sends 100 Continue when the handler begins reading the body.
	// No body bytes follow, so this upload is known to be stalled during the GET.
	if err := stalled.SetDeadline(time.Time{}); err != nil {
		t.Fatalf("clear upload handshake deadline: %v", err)
	}
	transport := &http.Transport{DisableKeepAlives: true}
	t.Cleanup(transport.CloseIdleConnections)
	client := &http.Client{Transport: transport, Timeout: 2 * time.Second}
	request, err := http.NewRequest(http.MethodGet, server.URL+"/v1/records", nil)
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Authorization", "Bearer "+apiTestToken)
	response, err := client.Do(request)
	if err != nil {
		t.Fatalf("GET on a separate connection was blocked by the incomplete POST: %v", err)
	}
	defer func() { _ = response.Body.Close() }()
	if response.StatusCode != http.StatusOK {
		t.Fatalf("GET records status = %d, want 200", response.StatusCode)
	}
	var list struct {
		Items []json.RawMessage `json:"items"`
		Total int               `json:"total"`
	}
	if err := json.NewDecoder(response.Body).Decode(&list); err != nil {
		t.Fatalf("decode records response: %v", err)
	}
	if list.Total != 0 || len(list.Items) != 0 {
		t.Errorf("incomplete POST created records: %+v", list)
	}
}
