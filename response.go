// Copyright 2016-2019 Alex Stocks, Yincheng Fang
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package hessian

import (
	"encoding/binary"
	"math"
	"reflect"
	"strconv"
	"strings"
)

import (
	perrors "github.com/pkg/errors"
)

import (
	"github.com/apache/dubbo-go-hessian2/java_exception"
)

type Response struct {
	RspObj      interface{}
	Exception   error
	Attachments map[string]string
}

// NewResponse create a new Response
func NewResponse(rspObj interface{}, exception error, attachments map[string]string) *Response {
	if attachments == nil {
		attachments = make(map[string]string)
	}
	return &Response{
		RspObj:      rspObj,
		Exception:   exception,
		Attachments: attachments,
	}
}

func EnsureResponse(body interface{}) *Response {
	if res, ok := body.(*Response); ok {
		return res
	}
	if exp, ok := body.(error); ok {
		return NewResponse(nil, exp, nil)
	}
	return NewResponse(body, nil, nil)
}

// dubbo-remoting/dubbo-remoting-api/src/main/java/com/alibaba/dubbo/remoting/exchange/codec/ExchangeCodec.java
// v2.7.1 line 256 encodeResponse
// hessian encode response
func packResponse(header DubboHeader, ret interface{}) ([]byte, error) {
	var (
		byteArray []byte
	)

	response := EnsureResponse(ret)

	hb := header.Type == PackageHeartbeat

	// magic
	if hb {
		byteArray = append(byteArray, DubboResponseHeartbeatHeader[:]...)
	} else {
		byteArray = append(byteArray, DubboResponseHeaderBytes[:]...)
	}
	// set serialID, identify serialization types, eg: fastjson->6, hessian2->2
	byteArray[2] |= header.SerialID & SERIAL_MASK
	// response status
	if header.ResponseStatus != 0 {
		byteArray[3] = header.ResponseStatus
	}

	// request id
	binary.BigEndian.PutUint64(byteArray[4:], uint64(header.ID))

	// body
	encoder := NewEncoder()
	encoder.Append(byteArray[:HEADER_LENGTH])

	if header.ResponseStatus == Response_OK {
		if hb {
			encoder.Encode(nil)
		} else {
			// com.alibaba.dubbo.rpc.protocol.dubbo.DubboCodec.DubboCodec.java
			// v2.7.1 line191 encodeResponseData

			atta := isSupportResponseAttachment(response.Attachments[DUBBO_VERSION_KEY])

			var resWithException, resValue, resNullValue int32
			if atta {
				resWithException = RESPONSE_WITH_EXCEPTION_WITH_ATTACHMENTS
				resValue = RESPONSE_VALUE_WITH_ATTACHMENTS
				resNullValue = RESPONSE_NULL_VALUE_WITH_ATTACHMENTS
			} else {
				resWithException = RESPONSE_WITH_EXCEPTION
				resValue = RESPONSE_VALUE
				resNullValue = RESPONSE_NULL_VALUE
			}

			if response.Exception != nil { // throw error
				encoder.Encode(resWithException)
				if t, ok := response.Exception.(java_exception.Throwabler); ok {
					encoder.Encode(t)
				} else {
					encoder.Encode(java_exception.NewThrowable(response.Exception.Error()))
				}
			} else {
				if response.RspObj == nil {
					encoder.Encode(resNullValue)
				} else {
					encoder.Encode(resValue)
					encoder.Encode(response.RspObj) // result
				}
			}

			if atta {
				encoder.Encode(response.Attachments) // attachments
			}
		}
	} else {
		// com.alibaba.dubbo.remoting.exchange.codec.ExchangeCodec
		// v2.6.5 line280 encodeResponse
		if response.Exception != nil { // throw error
			encoder.Encode(response.Exception.Error())
		} else {
			encoder.Encode(response.RspObj)
		}
	}

	byteArray = encoder.Buffer()
	byteArray = encNull(byteArray) // if not, "java client" will throw exception  "unexpected end of file"
	pkgLen := len(byteArray)
	if pkgLen > int(DEFAULT_LEN) { // 8M
		return nil, perrors.Errorf("Data length %d too large, max payload %d", pkgLen, DEFAULT_LEN)
	}
	// byteArray{body length}
	binary.BigEndian.PutUint32(byteArray[12:], uint32(pkgLen-HEADER_LENGTH))
	return byteArray, nil

}

// hessian decode response body
func unpackResponseBody(buf []byte, resp interface{}) error {
	// body
	decoder := NewDecoder(buf[:])
	rspType, err := decoder.Decode()
	if err != nil {
		return perrors.WithStack(err)
	}

	response := EnsureResponse(resp)

	switch rspType {
	case RESPONSE_WITH_EXCEPTION, RESPONSE_WITH_EXCEPTION_WITH_ATTACHMENTS:
		expt, err := decoder.Decode()
		if err != nil {
			return perrors.WithStack(err)
		}
		if rspType == RESPONSE_WITH_EXCEPTION_WITH_ATTACHMENTS {
			attachments, err := decoder.Decode()
			if err != nil {
				return perrors.WithStack(err)
			}
			atta, ok := attachments.(map[string]string)
			if ok {
				response.Attachments = atta
			} else {
				return perrors.Errorf("get wrong attachments: %+v", atta)
			}
		}

		if e, ok := expt.(error); ok {
			response.Exception = e
		} else {
			response.Exception = perrors.Errorf("got exception: %+v", expt)
		}
		return nil

	case RESPONSE_VALUE, RESPONSE_VALUE_WITH_ATTACHMENTS:
		rsp, err := decoder.Decode()
		if err != nil {
			return perrors.WithStack(err)
		}
		if rspType == RESPONSE_VALUE_WITH_ATTACHMENTS {
			attachments, err := decoder.Decode()
			if err != nil {
				return perrors.WithStack(err)
			}
			atta, ok := attachments.(map[string]string)
			if ok {
				response.Attachments = atta
			} else {
				return perrors.Errorf("get wrong attachments: %+v", atta)
			}
		}

		return perrors.WithStack(ReflectResponse(rsp, response.RspObj))

	case RESPONSE_NULL_VALUE, RESPONSE_NULL_VALUE_WITH_ATTACHMENTS:
		if rspType == RESPONSE_NULL_VALUE_WITH_ATTACHMENTS {
			attachments, err := decoder.Decode()
			if err != nil {
				return perrors.WithStack(err)
			}
			atta, ok := attachments.(map[string]string)
			if ok {
				response.Attachments = atta
			} else {
				return perrors.Errorf("get wrong attachments: %+v", atta)
			}
		}
		return nil
	}

	return nil
}

// CopySlice copy from inSlice to outSlice
func CopySlice(inSlice, outSlice reflect.Value) error {
	if inSlice.IsNil() {
		return perrors.New("@in is nil")
	}
	if inSlice.Kind() != reflect.Slice {
		return perrors.Errorf("@in is not slice, but %v", inSlice.Kind())
	}

	for outSlice.Kind() == reflect.Ptr {
		outSlice = outSlice.Elem()
	}

	size := inSlice.Len()
	outSlice.Set(reflect.MakeSlice(outSlice.Type(), size, size))

	for i := 0; i < size; i++ {
		inSliceValue := inSlice.Index(i)
		if !inSliceValue.Type().AssignableTo(outSlice.Index(i).Type()) {
			return perrors.Errorf("in element type [%s] can not assign to out element type [%s]",
				inSliceValue.Type().String(), outSlice.Type().String())
		}
		outSlice.Index(i).Set(inSliceValue)
	}

	return nil
}

// CopyMap copy from in map to out map
func CopyMap(inMapValue, outMapValue reflect.Value) error {
	if inMapValue.IsNil() {
		return perrors.New("@in is nil")
	}
	if !inMapValue.CanInterface() {
		return perrors.New("@in's Interface can not be used.")
	}
	if inMapValue.Kind() != reflect.Map {
		return perrors.Errorf("@in is not map, but %v", inMapValue.Kind())
	}

	outMapType := UnpackPtrType(outMapValue.Type())
	SetValue(outMapValue, reflect.MakeMap(outMapType))

	outKeyType := outMapType.Key()

	outMapValue = UnpackPtrValue(outMapValue)
	outValueType := outMapValue.Type().Elem()

	for _, inKey := range inMapValue.MapKeys() {
		inValue := inMapValue.MapIndex(inKey)

		if !inKey.Type().AssignableTo(outKeyType) {
			return perrors.Errorf("in Key:{type:%s, value:%#v} can not assign to out Key:{type:%s} ",
				inKey.Type().String(), inKey, outKeyType.String())
		}
		if !inValue.Type().AssignableTo(outValueType) {
			return perrors.Errorf("in Value:{type:%s, value:%#v} can not assign to out value:{type:%s}",
				inValue.Type().String(), inValue, outValueType.String())
		}
		outMapValue.SetMapIndex(inKey, inValue)
	}

	return nil
}

// ReflectResponse reflect return value
// TODO response object should not be copied again to another object, it should be the exact type of the object
func ReflectResponse(in interface{}, out interface{}) error {
	if in == nil {
		return perrors.Errorf("@in is nil")
	}

	if out == nil {
		return perrors.Errorf("@out is nil")
	}
	if reflect.TypeOf(out).Kind() != reflect.Ptr {
		return perrors.Errorf("@out should be a pointer")
	}

	inValue := EnsurePackValue(in)
	outValue := EnsurePackValue(out)

	outType := outValue.Type().String()
	if outType == "interface {}" || outType == "*interface {}" {
		SetValue(outValue, inValue)
		return nil
	}

	switch inValue.Type().Kind() {
	case reflect.Slice, reflect.Array:
		return CopySlice(inValue, outValue)
	case reflect.Map:
		return CopyMap(inValue, outValue)
	default:
		SetValue(outValue, inValue)
	}

	return nil
}

var versionInt = make(map[string]int)

// isSupportResponseAttachment is for compatibility among some dubbo version
// but we haven't used it yet.
// dubbo-common/src/main/java/org/apache/dubbo/common/Version.java
// v2.7.1 line 96
func isSupportResponseAttachment(version string) bool {
	if version == "" {
		return false
	}

	v, ok := versionInt[version]
	if !ok {
		v = version2Int(version)
		if v == -1 {
			return false
		}
	}

	if v >= 2001000 && v <= 2060200 { // 2.0.10 ~ 2.6.2
		return false
	}
	return v >= LOWEST_VERSION_FOR_RESPONSE_ATTACHMENT
}

func version2Int(version string) int {
	var v = 0
	varr := strings.Split(version, ".")
	length := len(varr)
	for key, value := range varr {
		v0, err := strconv.Atoi(value)
		if err != nil {
			return -1
		}
		v += v0 * int(math.Pow10((length-key-1)*2))
	}
	if length == 3 {
		return v * 100
	}
	return v
}
