/*
 Copyright 2021 The CI/CD Operator Authors

 Licensed under the Apache License, Version 2.0 (the "License");
 you may not use this file except in compliance with the License.
 You may obtain a copy of the License at

     http://www.apache.org/licenses/LICENSE-2.0

 Unless required by applicable law or agreed to in writing, software
 distributed under the License is distributed on an "AS IS" BASIS,
 WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
 See the License for the specific language governing permissions and
 limitations under the License.
*/

package git

import (
	"bytes"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"io/ioutil"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// GetPaginatedRequest gets paginated APIs and accumulates them together
func GetPaginatedRequest(apiURL string, tlsConfig *tls.Config, header map[string]string, newObj func() interface{}, accumulate func(interface{})) error {
	u, err := url.Parse(apiURL)
	if err != nil {
		return err
	}
	if u.RawQuery == "" {
		u.RawQuery = "per_page=100"
	} else {
		u.RawQuery += "&per_page=100"
	}
	uri := u.String()
	for {
		data, h, err := RequestHTTP(http.MethodGet, uri, header, nil, tlsConfig)
		if err != nil {
			return err
		}

		tmpObj := newObj()
		if err := json.Unmarshal(data, tmpObj); err != nil {
			return err
		}

		accumulate(tmpObj)

		links := ParseLinkHeader(h.Get("Link"))
		if links == nil {
			break
		}
		next := links.Find("next")
		if next == nil {
			break
		}
		uri = next.URL
	}

	return nil
}

// RequestHTTP requests api call
func RequestHTTP(method string, uri string, header map[string]string, data interface{}, tlsConfig *tls.Config) ([]byte, http.Header, error) {
	var jsonBytes []byte
	var err error

	if data != nil {
		jsonBytes, err = json.Marshal(data)
		if err != nil {
			return nil, nil, err
		}
	}

	req, err := http.NewRequest(method, uri, bytes.NewBuffer(jsonBytes))
	if err != nil {
		return nil, nil, err
	}

	for k, v := range header {
		req.Header.Add(k, v)
	}

	var resp *http.Response

	if tlsConfig != nil {
		tr := &http.Transport{
			TLSClientConfig: tlsConfig,
		}
		tlsClient := http.Client{Transport: tr}

		resp, err = tlsClient.Do(req)
		if err != nil {
			return nil, nil, err
		}
	} else {
		resp, err = http.DefaultClient.Do(req)
		if err != nil {
			return nil, nil, err
		}
	}

	defer func() {
		_ = resp.Body.Close()
	}()

	body, err := ioutil.ReadAll(resp.Body)
	if err != nil {
		return nil, nil, err
	}

	// Check additional response header
	var newErr error
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		newErr = fmt.Errorf("error requesting api [%s] %s, code %d, msg %s", method, uri, resp.StatusCode, string(body))
	}
	return body, resp.Header, newErr
}

// CheckRateLimitGetResetTime checks if the error is a rate limit exceeded error and return time at which limit is reset
func CheckRateLimitGetResetTime(err error) int {
	if err != nil && strings.Contains(err.Error(), "Rate limit exceeded") {
		strErr := err.Error()
		unixTime := strings.Split(strings.Split(strErr, "::")[1], ".")[0]
		if tm, convertErr := strconv.Atoi(unixTime); convertErr == nil {
			return tm
		}
	}
	return 0
}

// GetGapTime return target time - current time
func GetGapTime(target int) int64 {
	return int64(target) - time.Now().Unix()
}
