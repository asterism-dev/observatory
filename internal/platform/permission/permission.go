// Package permission reads the observatory.v1.permission method annotation.
package permission

import (
	"regexp"

	obsv1 "github.com/asterism-dev/observatory/gen/go/observatory/v1"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/types/descriptorpb"
)

type Permission struct {
	Name   string
	Object string
	Write  bool
	// AnyMember admits every org member without a rule check.
	AnyMember bool
}

var nameRE = regexp.MustCompile(`^[a-z][a-z0-9]*\.[a-z][a-zA-Z0-9]*\.[a-z][a-zA-Z0-9]*$`)

func ValidName(s string) bool { return nameRE.MatchString(s) }

func Of(md protoreflect.MethodDescriptor) (Permission, bool) {
	opts, _ := md.Options().(*descriptorpb.MethodOptions)
	if opts == nil || !proto.HasExtension(opts, obsv1.E_Permission) {
		return Permission{}, false
	}
	p, _ := proto.GetExtension(opts, obsv1.E_Permission).(*obsv1.Permission)
	if p == nil {
		return Permission{}, false
	}
	switch p.GetKind() {
	case obsv1.Kind_KIND_WRITE:
		return Permission{Name: p.GetName(), Object: p.GetObject(), Write: true, AnyMember: p.GetAnyMember()}, true
	case obsv1.Kind_KIND_READ:
		return Permission{Name: p.GetName(), Object: p.GetObject(), AnyMember: p.GetAnyMember()}, true
	}
	// An unset kind must never fail open as a read.
	return Permission{}, false
}
