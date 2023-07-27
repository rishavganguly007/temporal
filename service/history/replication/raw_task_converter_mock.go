// The MIT License
//
// Copyright (c) 2020 Temporal Technologies Inc.  All rights reserved.
//
// Copyright (c) 2020 Uber Technologies, Inc.
//
// Permission is hereby granted, free of charge, to any person obtaining a copy
// of this software and associated documentation files (the "Software"), to deal
// in the Software without restriction, including without limitation the rights
// to use, copy, modify, merge, publish, distribute, sublicense, and/or sell
// copies of the Software, and to permit persons to whom the Software is
// furnished to do so, subject to the following conditions:
//
// The above copyright notice and this permission notice shall be included in
// all copies or substantial portions of the Software.
//
// THE SOFTWARE IS PROVIDED "AS IS", WITHOUT WARRANTY OF ANY KIND, EXPRESS OR
// IMPLIED, INCLUDING BUT NOT LIMITED TO THE WARRANTIES OF MERCHANTABILITY,
// FITNESS FOR A PARTICULAR PURPOSE AND NONINFRINGEMENT. IN NO EVENT SHALL THE
// AUTHORS OR COPYRIGHT HOLDERS BE LIABLE FOR ANY CLAIM, DAMAGES OR OTHER
// LIABILITY, WHETHER IN AN ACTION OF CONTRACT, TORT OR OTHERWISE, ARISING FROM,
// OUT OF OR IN CONNECTION WITH THE SOFTWARE OR THE USE OR OTHER DEALINGS IN
// THE SOFTWARE.

// Code generated by MockGen. DO NOT EDIT.
// Source: raw_task_converter.go

// Package replication is a generated GoMock package.
package replication

import (
	reflect "reflect"

	gomock "github.com/golang/mock/gomock"
	repication "go.temporal.io/server/api/replication/v1"
	tasks "go.temporal.io/server/service/history/tasks"
)

// MockSourceTaskConverter is a mock of SourceTaskConverter interface.
type MockSourceTaskConverter struct {
	ctrl     *gomock.Controller
	recorder *MockSourceTaskConverterMockRecorder
}

// MockSourceTaskConverterMockRecorder is the mock recorder for MockSourceTaskConverter.
type MockSourceTaskConverterMockRecorder struct {
	mock *MockSourceTaskConverter
}

// NewMockSourceTaskConverter creates a new mock instance.
func NewMockSourceTaskConverter(ctrl *gomock.Controller) *MockSourceTaskConverter {
	mock := &MockSourceTaskConverter{ctrl: ctrl}
	mock.recorder = &MockSourceTaskConverterMockRecorder{mock}
	return mock
}

// EXPECT returns an object that allows the caller to indicate expected use.
func (m *MockSourceTaskConverter) EXPECT() *MockSourceTaskConverterMockRecorder {
	return m.recorder
}

// Convert mocks base method.
func (m *MockSourceTaskConverter) Convert(task tasks.Task) (*repication.ReplicationTask, error) {
	m.ctrl.T.Helper()
	ret := m.ctrl.Call(m, "Convert", task)
	ret0, _ := ret[0].(*repication.ReplicationTask)
	ret1, _ := ret[1].(error)
	return ret0, ret1
}

// Convert indicates an expected call of Convert.
func (mr *MockSourceTaskConverterMockRecorder) Convert(task interface{}) *gomock.Call {
	mr.mock.ctrl.T.Helper()
	return mr.mock.ctrl.RecordCallWithMethodType(mr.mock, "Convert", reflect.TypeOf((*MockSourceTaskConverter)(nil).Convert), task)
}