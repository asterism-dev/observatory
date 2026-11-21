package authz

import (
	"errors"
	"fmt"
	"strings"

	"connectrpc.com/connect"
	"github.com/asterism-dev/observatory/internal/platform/uuidx"
	"github.com/google/uuid"
	"google.golang.org/protobuf/reflect/protoreflect"
)

func errBadAnnotation() error {
	return connect.NewError(connect.CodeUnimplemented, errors.New("method has no valid permission annotation"))
}

// ResolveObject replaces {self} with the caller first (never a request field), then fills each other {field} of pattern from a singular string field of m holding a canonical UUID.
func ResolveObject(pattern string, self uuid.UUID, m protoreflect.Message) (string, error) {
	var b strings.Builder
	rest := strings.ReplaceAll(pattern, "{self}", self.String())
	for {
		before, after, found := strings.Cut(rest, "{")
		if strings.Contains(before, "}") {
			return "", errBadAnnotation()
		}
		b.WriteString(before)
		if !found {
			return b.String(), nil
		}
		name, tail, ok := strings.Cut(after, "}")
		if !ok {
			return "", errBadAnnotation()
		}
		fd := m.Descriptor().Fields().ByName(protoreflect.Name(name))
		if fd == nil || fd.Kind() != protoreflect.StringKind || fd.Cardinality() == protoreflect.Repeated {
			return "", errBadAnnotation()
		}
		v := m.Get(fd).String()
		if _, err := uuidx.Canonical(v); err != nil {
			return "", connect.NewError(connect.CodeInvalidArgument, fmt.Errorf("%s: %w", name, err))
		}
		b.WriteString(v)
		rest = tail
	}
}
