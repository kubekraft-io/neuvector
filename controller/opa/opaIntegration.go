package opa

import (
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"io/ioutil"
	"net/http"
	"os"
	"os/exec"
	"strings"
	"time"

	log "github.com/sirupsen/logrus"
)

type OpaRuleHead struct {
	Name string `json:"name"`
}

type OpaRule struct {
	Head OpaRuleHead
}

type OpaPackageItem struct {
	Type  string `json:"type"`
	Value string `json:"value"`
}

type OpaPackage struct {
	Items []OpaPackageItem `json:"path"`
}

type OpaPolicyAst struct {
	Rules    []OpaRule  `json:"rules"`
	Packages OpaPackage `json:"package"`
}

type OpaPolicy struct {
	ID  string `json:"id"`
	Raw string `json:"raw"`
	// Ast OpaPolicyAst `json:"ast"`	// we don't need this part
}

type OpaPolicyResultList struct {
	Result []OpaPolicy `json:"result"`
}

type OpaPolicyResult struct {
	Result OpaPolicy `json:"result"`
}

//
// for spec reading..
type EvaluationResultSpec struct {
	Spec RegoSpecification `json:"specification"`
}

type OpaEvalResultSpec struct {
	Result EvaluationResultSpec `json:"result"`
}

// for evaluation result
type RegoResultEntryV1 struct {
	Message     string `json:"message"`
	CustomField string `json:"additional_message,omitempty"`
}

type RegoSpecification struct {
	Version     string `json:"version"`
	Description string `json:"description"`
}

// for spec.Version=="v1"
type EvaluationResultV1 struct {
	Violations    []RegoResultEntryV1 `json:"violation"`
	ViolationMsgs []string            `json:"violationmsgs"`
	Spec          RegoSpecification   `json:"specification"`
}

type OpaEvalResultV1 struct {
	Result EvaluationResultV1 `json:"result"`
}

// this program is expected to compiled and run in Linux
// the sqlite3 seems has compiling issue on Windows platform
var (
	// TLS version
	// opaServer = "https://localhost:8181"

	opaServer = "http://localhost:8181"
)

var UseOPA_HTTPS = false
var opaCacheDoc map[string]string
var opaCachePolicy map[string]string

var (
	ContentTypeJson = "application/json; charset=utf-8"
	ContentTypeText = "text/plain; charset=utf-8"
)

func StartOpaServer() *exec.Cmd {
	var cmd *exec.Cmd
	if UseOPA_HTTPS {
		// https
		// opa run --server --log-level debug --tls-cert-file public.crt --tls-private-key-file private.key --addr=:8181
		cmd = exec.Command("./opa_binary/opa_linux_amd64_static", "run", "--server", "--ignore=.*", "--tls-cert-file", "./opa_binary/opa_cert/public.crt", "--tls-private-key-file", "./opa_binary/opa_cert/private.key", "--addr=:8181")
	} else {
		// http
		// cmd = exec.Command("./opa_binary/opa_linux_amd64_static", "run", "--server", "--ignore=.*", "--addr=:8181")
		cmd = exec.Command("/usr/local/bin/opa", "run", "--server", "--ignore=.*", "--addr=:8181", "--log-level=error")
	}

	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	err := cmd.Start() // *** use Start()
	if err != nil {
		log.WithFields(log.Fields{
			"error": err,
		}).Error("create opa failed")
		return nil
	} else {
		log.Info(fmt.Sprintf("run opa success, the pid is %d\n", cmd.Process.Pid))
		time.Sleep(2 * time.Second) // wait a while to let opa server start correctly..
	}

	if len(opaCacheDoc) == 0 {
		opaCacheDoc = make(map[string]string)
	}

	if len(opaCachePolicy) == 0 {
		opaCachePolicy = make(map[string]string)
	}

	return cmd
}

func getOpaHTTPClient() *http.Client {
	if UseOPA_HTTPS {
		transCfg := &http.Transport{
			TLSClientConfig: &tls.Config{InsecureSkipVerify: true}, // ignore expired SSL certificates
		}
		return &http.Client{Transport: transCfg}
	} else {
		return &http.Client{}
	}
}

func addObject(key string, contentType string, data string) bool {
	client := getOpaHTTPClient()

	// set the HTTP method, url, and request body
	req, err := http.NewRequest(http.MethodPut, fmt.Sprintf("%s%s", opaServer, key), strings.NewReader(data))
	if err != nil {
		log.WithFields(log.Fields{
			"key":         key,
			"contentType": contentType,
			"error":       err,
		}).Error("OPA addObject NewRequest")
		return false
	}

	req.Header.Set("Content-Type", contentType)
	resp, err := client.Do(req)
	if err != nil {
		log.WithFields(log.Fields{
			"key":         key,
			"contentType": contentType,
			"error":       err,
		}).Error("OPA addObject NewRequest - client.Do(req)")

		// restart OPA process and restore data
		if strings.Contains(err.Error(), "connection refused") {
			fmt.Printf("OPA server failed. Try to start !\n")
			StartOpaServer()
			RestoreOpaData()
		}

		return false
	}
	defer resp.Body.Close()

	_, readErr := ioutil.ReadAll(resp.Body)
	if readErr != nil {
		log.WithFields(log.Fields{
			"key":         key,
			"contentType": contentType,
			"error":       err,
		}).Error("OPA addObject NewRequest")
		return false
	}

	if resp.StatusCode == 200 || resp.StatusCode == 204 {
		return true
	}
	return false
}

func AddPolicy(key string, regoStr string) bool {
	result := addObject(key, ContentTypeText, regoStr)

	if result {
		opaCachePolicy[key] = regoStr
	}

	return result
}

func AddDocument(key string, jsonData string) bool {
	result := addObject(key, ContentTypeJson, jsonData)

	if result {
		opaCacheDoc[key] = jsonData
	}

	return result
}

func AddDocumentIfNotExist(key string, jsonData string) bool {
	// step-1: if key exist
	client := getOpaHTTPClient()

	// get populated policies
	url := fmt.Sprintf("%s%s", opaServer, key)
	resp, getErr := client.Get(url)
	if getErr != nil {
		log.WithFields(log.Fields{
			"url":   url,
			"error": getErr,
		}).Error("OPA request")
		return false
	}

	defer resp.Body.Close()

	body, readErr := ioutil.ReadAll(resp.Body)
	if readErr != nil {
		log.WithFields(log.Fields{
			"error": readErr,
		}).Error("OPA error on ReadAll")
		return false
	}

	if resp.StatusCode == http.StatusOK && strings.Contains(string(body), "result") {
		// exist, return without add
		return true
	} else {
		// not exist... we can add
		return addObject(key, ContentTypeJson, jsonData)
	}
}

func DeletePolicy(ruleId uint32) {
	DeleteDocument(formatPolicyUrl(ruleId))
}

func DeleteDocument(key string) {
	client := getOpaHTTPClient()

	req, err := http.NewRequest(http.MethodDelete, fmt.Sprintf("%s%s", opaServer, key), nil)
	if err != nil {
		log.WithFields(log.Fields{
			"key":   key,
			"error": err,
		}).Error("OPA delDocument NewRequest")
		return
	}

	resp, err := client.Do(req)
	if err != nil {
		log.WithFields(log.Fields{
			"key":   key,
			"error": err,
		}).Error("OPA delDocument NewRequest")
		return
	}
	defer resp.Body.Close()

	body, readErr := ioutil.ReadAll(resp.Body)
	if readErr != nil {
		log.WithFields(log.Fields{
			"key":   key,
			"error": readErr,
		}).Error("OPA delDocument NewRequest")
		return
	}

	log.WithFields(log.Fields{
		"key":        key,
		"StatusCode": resp.StatusCode,
		"body":       string(body),
	}).Info("OPA delDocument returns")

	delete(opaCacheDoc, key)
	delete(opaCachePolicy, key)
}

func OpaGetPolicies() []OpaPolicy {
	client := getOpaHTTPClient()

	// get populated policies
	url := fmt.Sprintf("%s%s", opaServer, "/v1/policies")
	resp, getErr := client.Get(url)
	if getErr != nil {
		log.WithFields(log.Fields{
			"url":   url,
			"error": getErr,
		}).Error("OpaGetPolicies get error")
		return nil
	}
	defer resp.Body.Close()

	body, readErr := ioutil.ReadAll(resp.Body)
	if readErr != nil {
		log.WithFields(log.Fields{
			"error": readErr,
		}).Error("OpaGetPolicies error on ReadAll")
		return nil
	}

	var results OpaPolicyResultList
	if err := json.Unmarshal(body, &results); err != nil {
		log.WithFields(log.Fields{
			"error": err,
		}).Error("OpaGetPolicies json.Unmarshal to []OpaPolicy failed")
		return nil
	}

	return results.Result
}

func OpaEval(policyPath string, inputFile string) (int, string, error) {
	bytes, err := ioutil.ReadFile(inputFile)
	if err != nil {
		return -1, "", err
	}

	return OpaEvalByString(policyPath, string(bytes))
}

func OpaEvalByString(policyPath string, inputData string) (int, string, error) {
	client := getOpaHTTPClient()

	// set the HTTP method, url, and request body
	req, err := http.NewRequest(http.MethodPost, fmt.Sprintf("%s%s", opaServer, policyPath), strings.NewReader(inputData))
	if err != nil {
		return -1, "", err
	}

	// set the request header Content-Type for json
	req.Header.Set("Content-Type", "application/json; charset=utf-8")
	resp, err := client.Do(req)
	if err != nil {
		// restart OPA process and restore data
		if strings.Contains(err.Error(), "connection refused") {
			fmt.Printf("OPA server failed. Try to start !\n")
			StartOpaServer()
			RestoreOpaData()
		}
		return -1, "", err
	}
	defer resp.Body.Close()

	body, readErr := ioutil.ReadAll(resp.Body)
	if readErr != nil {
		return resp.StatusCode, "", err
	}

	return resp.StatusCode, string(body), nil
}

func RestoreOpaData() {
	for k, v := range opaCacheDoc {
		AddDocument(k, v)
	}

	for k, v := range opaCachePolicy {
		AddPolicy(k, v)
	}
}

// this function should return matched or not
// needs to return same information as we need in NeuVector controller webhook..
func AnalyzeResult(response string) (bool, error) {
	// check the spec first
	var spec OpaEvalResultSpec
	if err := json.Unmarshal([]byte(response), &spec); err != nil {
		log.WithFields(log.Fields{
			"error": err,
		}).Error("AnalyzeResult Unmarshal() to get spec.")
		return false, errors.New("rego execution error, no specification found")
	}

	log.WithFields(log.Fields{
		"spec": spec,
	}).Info("AnalyzeResult spec.")

	if spec.Result.Spec.Version == "v1" {
		var results OpaEvalResultV1 // ## define a version,,   like OpaEvalResultV1 for extensibility.., and have a data field to indicate what version we should use
		if err := json.Unmarshal([]byte(response), &results); err != nil {
			log.WithFields(log.Fields{
				"spec":  spec,
				"error": err,
			}).Error("AnalyzeResult Unmarshal()")
			return false, errors.New("rego execution error, unable to parse to v1 specification")
		}

		// show violations
		fmt.Printf("violations:\n")
		for i, v := range results.Result.Violations {
			fmt.Printf("	[%d] %s\n", i, v.Message)
		}

		// show violationmsgs
		fmt.Printf("violationmsgs:\n")
		for i, v := range results.Result.ViolationMsgs {
			fmt.Printf("	[%d] %s\n", i, v)
		}

		return len(results.Result.Violations) > 0, nil
	} else {
		log.WithFields(log.Fields{
			"spec": spec,
		}).Error("AnalyzeResult unsupported spec.")
		return false, errors.New("rego execution error, unsupported spec")
	}
}

func GetRiskyRoleRuleIDByName(ruleName string) int {
	client := getOpaHTTPClient()

	// get the base64 string
	mappingKey := FormatRiskyRuleMappingKey(ruleName)

	url := fmt.Sprintf("%s%s", opaServer, mappingKey)
	resp, getErr := client.Get(url)
	if getErr != nil {
		log.WithFields(log.Fields{
			"url":   url,
			"error": getErr,
		}).Error("GetRiskyRoleRuleIDByName get error")
		return 0
	}
	defer resp.Body.Close()

	body, readErr := ioutil.ReadAll(resp.Body)
	if readErr != nil {
		log.WithFields(log.Fields{
			"url":   url,
			"error": getErr,
		}).Error("GetRiskyRoleRuleIDByName error on ReadAll")
		return 0
	}

	fmt.Printf("[GetRiskyRoleRuleIDByName] url=%s, body=%s\n", url, string(body))

	type MappingRuleId struct {
		RuldID int `json:"ruleid"`
	}

	response := struct {
		Result MappingRuleId `json:"result"`
	}{}

	if err := json.Unmarshal(body, &response); err != nil {
		fmt.Printf("json.Unmarshal to OpaPolicy failed. %v\n", err)
	}

	return response.Result.RuldID
}
