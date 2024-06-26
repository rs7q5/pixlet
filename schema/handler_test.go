package schema_test

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"tidbyt.dev/pixlet/runtime"
)

var handlerSource = `
load("schema.star", "schema")

def assert(success, message=None):
    if not success:
        fail(message or "assertion failed")

def foobar(param):
    return "derp"

h = schema.Handler(
    handler = foobar,
    type = schema.HandlerType.String,
)

assert(h.handler == foobar)
assert(h.type == schema.HandlerType.String)

def main():
	return []
`

func TestHandler(t *testing.T) {
	app, err := runtime.NewApplet("handler.star", []byte(handlerSource))
	assert.NoError(t, err)

	screens, err := app.Run(context.Background())
	assert.NoError(t, err)
	assert.NotNil(t, screens)
}

func TestHandlerBadParams(t *testing.T) {
	// Handler is a string
	app, err := runtime.NewApplet("text.star", []byte(`
load("schema.star", "schema")

def foobar(param):
    return "derp"

h = schema.Handler(
    handler = "foobar",
    type = schema.HandlerType.String,
)

def main():
	return []
`))
	assert.Error(t, err)
	assert.Nil(t, app)

	// Type is not valid
	app, err = runtime.NewApplet("text.star", []byte(`
load("schema.star", "schema")

def foobar(param):
    return "derp"

h = schema.Handler(
    handler = foobar,
    type = 42,
)

def main():
	return []
`))
	assert.Error(t, err)
	assert.Nil(t, app)
}
