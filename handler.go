package main

import (
	"encoding/json"
	"fmt"
	"io/ioutil"
	"math"
	"net/http"
	"reflect"
	"strconv"
	"strings"
	"time"

	"github.com/LeKovr/dbrpc/workman"
	"github.com/LeKovr/go-base/logger"
)

// -----------------------------------------------------------------------------

// ArgDef holds function argument attributes
type ArgDef struct {
	ID        int32   `json:"id"`
	Name      string  `json:"arg"`
	Type      string  `json:"type"`
	Default   *string `json:"def"`
	DefIsNull bool    `json:"def_is_null"`
}

// FuncArgDef holds slice of function argument attributes
type FuncArgDef []ArgDef

// RPCServer holds server attributes
type RPCServer struct {
	cfg   *AplFlags
	log   *logger.Log
	jc    chan workman.Job
	funcs *FuncMap
}

// -----------------------------------------------------------------------------

// JSON-RPC v2.0 structures
type reqParams map[string]interface{}

type serverRequest struct {
	Method  string    `json:"method"`
	Version string    `json:"jsonrpc"`
	ID      uint64    `json:"id"`
	Params  reqParams `json:"params"`
}

type serverResponse struct {
	ID      uint64           `json:"id"`
	Version string           `json:"jsonrpc"`
	Result  *json.RawMessage `json:"result,omitempty"`
	Error   *json.RawMessage `json:"error,omitempty"`
}

type respRPCError struct {
	Code    int              `json:"code"`
	Message string           `json:"message"`
	Data    *json.RawMessage `json:"data,omitempty"`
}
type respPGTError struct {
	Message string           `json:"message"`
	Code    string           `json:"code,omitempty"`
	Details *json.RawMessage `json:"details,omitempty"`
}

// -----------------------------------------------------------------------------

// FunctionArgDef creates a job for fetching of function argument definition
func (s RPCServer) FunctionArgDef(nsp, proc string) (FuncArgDef, interface{}) {

	key := []*string{nil, &s.cfg.ArgDefFunc, &nsp, &proc}

	payload, _ := json.Marshal(key)
	respChannel := make(chan workman.Result)

	work := workman.Job{Payload: string(payload), Result: respChannel}

	// Push the work onto the queue.
	s.jc <- work

	resp := <-respChannel
	s.log.Debugf("Got def (%v): %s", resp.Success, resp.Result)
	if !resp.Success {
		return nil, resp.Error
	}

	var res FuncArgDef
	err := json.Unmarshal(*resp.Result, &res)
	if err != nil {
		return nil, err
	}
	return res, nil
}

// -----------------------------------------------------------------------------

// FunctionResult creates a job for fetching of function result
func FunctionResult(jc chan workman.Job, payload string) workman.Result {

	respChannel := make(chan workman.Result)
	// let's create a job with the payload
	work := workman.Job{Payload: payload, Result: respChannel}

	// Push the work onto the queue.
	jc <- work

	resp := <-respChannel
	return resp
}

// -----------------------------------------------------------------------------

func (s RPCServer) httpHandler() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		log := s.log
		cfg := s.cfg
		defer r.Body.Close()
		log.Debugf("Request method: %s", r.Method)

		if origin := r.Header.Get("Origin"); origin != "" {

			log.Debugf("Lookup origin %s in %+v", origin, cfg.Hosts)
			if !originAllowed(cfg.Hosts, origin) {
				log.Warningf("Unregistered request source: %s", origin)
				http.Error(w, "Origin not registered", http.StatusForbidden)
				return
			}
			w.Header().Add("Access-Control-Allow-Origin", origin)
			w.Header().Add("Access-Control-Allow-Credentials", "true") // TODO
			w.Header().Add("Access-Control-Allow-Headers",
				"origin, content-type, accept, keep-alive, user-agent, x-requested-with, x-token")
			w.Header().Add("Access-Control-Allow-Methods", "GET, POST, OPTIONS")

		}

		if r.Method == "GET" {
			s.getContextHandler(w, r, true, cfg.Compact)
		} else if r.Method == "HEAD" {
			s.getContextHandler(w, r, false, false) // Like get but without data
		} else if r.Method == "POST" && r.URL.Path == cfg.Prefix {
			s.postContextHandler(w, r)
		} else if r.Method == "POST" {
			s.postgrestContextHandler(w, r)
		} else if r.Method == "OPTIONS" {
			w.Header().Add("Content-Type", "text/plain; charset=UTF-8")
			w.WriteHeader(http.StatusNoContent)
		} else {
			e := fmt.Sprintf("Unsupported request method: %s", r.Method)
			log.Warn(e)
			http.Error(w, e, http.StatusNotImplemented)
		}
	}
}

// -----------------------------------------------------------------------------

func setMetric(w http.ResponseWriter, start time.Time, status int) {
	w.Header().Set("Content-Type", "application/json; charset=UTF-8")
	w.Header().Set("X-Elapsed", fmt.Sprint(time.Since(start)))
	w.WriteHeader(status)
}

// -----------------------------------------------------------------------------

func getRaw(data interface{}) *json.RawMessage {
	j, _ := json.Marshal(data)
	raw := json.RawMessage(j)
	return &raw
}

// -----------------------------------------------------------------------------

func originAllowed(origins []string, origin string) bool {
	if len(origins) > 0 { // lookup if host is allowed
		for _, h := range origins {
			if h == "*" || origin == h {
				return true
			}
		}
	}
	return false
}

// FunctionDef returns function attributes from index() method
func (s RPCServer) FunctionDef(method string) (*FuncDef, error) {
	fm := *s.funcs

	if def, ok := fm[method]; ok {
		return &def, nil
	}
	return nil, fmt.Errorf("no method %s", method)
}

// -----------------------------------------------------------------------------

func (s RPCServer) getContextHandler(w http.ResponseWriter, r *http.Request, reply bool, compact bool) {
	start := time.Now()
	log := s.log
	method := strings.TrimPrefix(r.URL.Path, s.cfg.Prefix)
	method = strings.TrimSuffix(method, ".json") // Allow use .json in url
	fd, err := s.FunctionDef(method)
	if err != nil {
		// Warning was when fetched from db
		log.Infof("Method %s load def error: %s", method, err)
		http.NotFound(w, r)
		return
	}

	argDef, errd := s.FunctionArgDef(fd.NspName, fd.ProName)
	if errd != nil {
		// Warning was when fetched from db
		log.Infof("Method %s load def error: %s", method, errd)
		http.NotFound(w, r)
		return
	}

	//key := []*string{&fd.NspName, &fd.ProName}
	r.ParseForm()

	f404 := []string{}
	ret := CallDef{Name: &fd.NspName, Proc: &fd.ProName, Args: map[string]interface{}{}}

	for _, a := range argDef {
		v := r.Form[a.Name]
		if len(v) == 0 {
			if !a.DefIsNull && a.Default == nil {
				f404 = append(f404, a.Name)
			} else if a.Default != nil {
				log.Debugf("Arg: %s use default", a.Name)
			}
		} else if strings.HasSuffix(a.Type, "[]") {
			// convert array into string
			if v[0] == "{}" {
				// empty array
				ret.Args[a.Name] = &v[0]
			} else {
				s := "{" + strings.Join(v, ",") + "}" // TODO: escape ","
				ret.Args[a.Name] = &s
			}
		} else {
			ret.Args[a.Name] = &v[0]
		}
	}
	var result workman.Result
	if len(f404) > 0 {
		result = workman.Result{Success: false, Error: fmt.Sprintf("Required parameter(s) %+v not found", f404)}
	} else {
		payload, _ := json.Marshal(ret)
		log.Debugf("Args: %s", string(payload))
		result = FunctionResult(s.jc, string(payload))
	}

	if reply {
		var out []byte
		if compact {
			out, err = json.Marshal(result)
		} else {
			out, err = json.MarshalIndent(result, "", "    ")
		}
		if err != nil {
			log.Warnf("Marshall error: %+v", err)
			e := workman.Result{Success: false, Error: err.Error()}
			out, _ = json.Marshal(e)
		}
		setMetric(w, start, http.StatusOK)
		log.Debug("Start writing")
		w.Write(out)
		//w.Write([]byte("\n"))
	} else {
		w.WriteHeader(http.StatusOK)
	}
}

// -----------------------------------------------------------------------------

// postContextHandler serve JSON-RPC envelope
func (s RPCServer) postContextHandler(w http.ResponseWriter, r *http.Request) {

	start := time.Now()
	log := s.log

	data, _ := ioutil.ReadAll(r.Body)
	req := serverRequest{}
	err := json.Unmarshal(data, &req)
	if err != nil {
		e := fmt.Sprintf("json parse error: %s", err)
		log.Warn(e)
		http.Error(w, e, http.StatusBadRequest)
		return
	}

	resultRPC := serverResponse{ID: req.ID, Version: req.Version}
	resultStatus := http.StatusOK

	fd, err := s.FunctionDef(req.Method)
	if err != nil {
		resultRPC.Error = getRaw(respRPCError{Code: -32601, Message: "Method not found", Data: getRaw(err)})
		resultStatus = http.StatusNotFound
	} else {

		argDef, errd := s.FunctionArgDef(fd.NspName, fd.ProName)
		if errd != nil {
			log.Warnf("Method %s load def error: %s", req.Method, errd)
			resultRPC.Error = getRaw(respRPCError{Code: -32601, Message: "Method not found", Data: getRaw(errd)})
			resultStatus = http.StatusNotFound
		} else {
			// Load args
			r.ParseForm()
			log.Infof("Argument source: %+v", req.Params)
			key, f404 := fetchArgs(log, argDef, req.Params, fd.NspName, fd.ProName)
			if len(f404) > 0 {
				resultRPC.Error = getRaw(respRPCError{Code: -32602, Message: "Required parameter(s) not found", Data: getRaw(f404)})
			} else {
				payload, _ := json.Marshal(key)
				log.Debugf("Args: %s", string(payload))
				res := FunctionResult(s.jc, string(payload))
				if res.Success {
					resultRPC.Result = res.Result
				} else {
					resultRPC.Error = getRaw(respRPCError{Code: -32603, Message: "Internal Error", Data: getRaw(res.Error)})
				}
			}
		}
	}

	out, err := json.Marshal(resultRPC)
	if err != nil {
		log.Warnf("Marshall error: %+v", err)
		resultRPC.Result = nil
		resultRPC.Error = getRaw(respRPCError{Code: -32603, Message: "Internal Error", Data: getRaw(err.Error())})

		out, _ = json.Marshal(resultRPC)
	}
	setMetric(w, start, resultStatus)
	log.Debug("Start writing")
	w.Write(out)
	log.Debugf("JSON Resp: %s", string(out))
	//w.Write([]byte("\n"))
}

// -----------------------------------------------------------------------------

// postgrestContextHandler serve JSON-RPC envelope
// 404 when method not found
func (s RPCServer) postgrestContextHandler(w http.ResponseWriter, r *http.Request) {

	start := time.Now()
	log := s.log

	method := strings.TrimPrefix(r.URL.Path, s.cfg.Prefix)
	method = strings.TrimSuffix(method, ".json") // Allow use .json in url
	log.Debugf("postgrest call for %s", method)

	fd, err := s.FunctionDef(method)
	if err != nil {
		log.Warnf("Method %s load def error: %s", method, err)
		http.NotFound(w, r)
		return
	}

	argDef, errd := s.FunctionArgDef(fd.NspName, fd.ProName)
	if errd != nil {
		log.Warnf("Method %s load def error: %s", method, errd)
		http.NotFound(w, r)
		return
	}
	resultStatus := http.StatusOK

	req := reqParams{}
	var resultRPC interface{}

	data, _ := ioutil.ReadAll(r.Body)

	if len(data) == 0 {
		resultRPC = respPGTError{Message: "Cannot parse empty request payload, use '{}'"}
		resultStatus = http.StatusBadRequest
	} else {

		err = json.Unmarshal(data, &req)

		if err != nil {
			e := fmt.Sprintf("json parse error: %s", err)
			log.Warnf("Error parse request(%s): %+v", data, e)
			resultRPC = respPGTError{Message: "Cannot parse request payload", Details: getRaw(e)}
			resultStatus = http.StatusBadRequest
		} else {
			// Load args
			log.Infof("Argument source: %+v", req)
			key, f404 := fetchArgs(log, argDef, req, fd.NspName, fd.ProName)
			if len(f404) > 0 {
				resultRPC = respPGTError{Code: "42883", Message: "Required parameter(s) not found", Details: getRaw(strings.Join(f404, ", "))}
				resultStatus = http.StatusBadRequest
			} else {
				payload, _ := json.Marshal(key)
				log.Debugf("Args: %s", string(payload))
				res := FunctionResult(s.jc, string(payload))
				if res.Success {
					resultRPC = res.Result
				} else {
					resultRPC = respPGTError{Message: "Method call error", Details: getRaw(res.Error)}
					resultStatus = http.StatusBadRequest // TODO: ?
				}
			}
		}
	}

	out, err := json.Marshal(resultRPC)
	if err != nil {
		log.Warnf("Marshall error: %+v", err)
		resultRPC = respPGTError{Message: "Method result marshall error", Details: getRaw(err)}
		resultStatus = http.StatusBadRequest // TODO: ?
		out, _ = json.Marshal(resultRPC)
	}
	setMetric(w, start, resultStatus)
	log.Debug("Start writing")
	w.Write(out)
	log.Debugf("JSON Resp: %s", string(out))
	//w.Write([]byte("\n"))
}

func fetchArgs(log *logger.Log, argDef FuncArgDef, req reqParams, nsp, proc string) (CallDef, []string) {

	f404 := []string{}
	ret := CallDef{Name: &nsp, Proc: &proc, Args: map[string]interface{}{}}

	for _, a := range argDef {
		v, ok := req[a.Name]
		if !ok {
			if !a.DefIsNull && a.Default == nil {
				f404 = append(f404, a.Name)
			} else if a.Default != nil {
				log.Debugf("Arg: %s use default", a.Name)
			}
		} else if strings.HasSuffix(a.Type, "[]") {
			// wait slice
			s := reflect.ValueOf(v)
			if s.Kind() != reflect.Slice {
				// string or {string}
				log.Debugf("=Array from no slice: %+v", v)
				ret.Args[a.Name] = s //&vs
			} else {
				// slice
				arr := make([]string, s.Len())

				for i := 0; i < s.Len(); i++ {
					arr[i] = s.Index(i).Interface().(string)
					//	log.Printf("====== %+v", ret[i])
				}
				// convert array into string
				// TODO: escape ","
				ss := "{" + strings.Join(arr, ",") + "}"
				log.Debugf("=Array from slice: %+v", ss)
				ret.Args[a.Name] = &ss
			}
		} else {
			log.Debugf("=Scalar from iface: %+v", v)
			ret.Args[a.Name] = v
		}

	}
	return ret, f404
}

func getFloat(unk interface{}) (float64, error) {
	switch i := unk.(type) {
	case float64:
		return i, nil
	case float32:
		return float64(i), nil
	case int64:
		return float64(i), nil
	// ...other cases...
	default:
		return math.NaN(), fmt.Errorf("getFloat: unknown value is of incompatible type %+v", i)
	}
}

func getBool(unk interface{}) (bool, error) {
	switch unk.(type) {
	case bool:
		if unk.(bool) {
			return true, nil
		}
		return false, nil

	// ...other cases...
	default:
		b, err := strconv.ParseBool(unk.(string))
		return b, err //fmt.Errorf("getFloat: unknown value is of incompatible type %+v", i)
	}
}
