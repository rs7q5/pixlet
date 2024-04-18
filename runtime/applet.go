package runtime

import (
	"context"
	"encoding/json"
	"fmt"
	"io/fs"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"testing/fstest"

	starlibgzip "github.com/qri-io/starlib/compress/gzip"
	starlibbase64 "github.com/qri-io/starlib/encoding/base64"
	starlibcsv "github.com/qri-io/starlib/encoding/csv"
	starlibhash "github.com/qri-io/starlib/hash"
	starlibhtml "github.com/qri-io/starlib/html"
	starlibre "github.com/qri-io/starlib/re"
	starlibzip "github.com/qri-io/starlib/zipfile"
	starlibjson "go.starlark.net/lib/json"
	starlibmath "go.starlark.net/lib/math"
	starlibtime "go.starlark.net/lib/time"
	"go.starlark.net/starlark"
	"go.starlark.net/starlarkstruct"
	"go.starlark.net/starlarktest"
	"go.starlark.net/syntax"

	"tidbyt.dev/pixlet/render"
	"tidbyt.dev/pixlet/runtime/modules/animation_runtime"
	"tidbyt.dev/pixlet/runtime/modules/hmac"
	"tidbyt.dev/pixlet/runtime/modules/humanize"
	"tidbyt.dev/pixlet/runtime/modules/qrcode"
	"tidbyt.dev/pixlet/runtime/modules/random"
	"tidbyt.dev/pixlet/runtime/modules/render_runtime"
	"tidbyt.dev/pixlet/runtime/modules/starlarkhttp"
	"tidbyt.dev/pixlet/runtime/modules/sunrise"
	"tidbyt.dev/pixlet/runtime/modules/xpath"
	"tidbyt.dev/pixlet/schema"
	"tidbyt.dev/pixlet/starlarkutil"
)

type ModuleLoader func(*starlark.Thread, string) (starlark.StringDict, error)

type PrintFunc func(thread *starlark.Thread, msg string)

type AppletOption func(*Applet) error

// ThreadInitializer is called when building a Starlark thread to run an applet
// on. It can customize the thread by overriding behavior or attaching thread
// local data.
type ThreadInitializer func(thread *starlark.Thread) *starlark.Thread

type Applet struct {
	ID string

	loader       ModuleLoader
	initializers []ThreadInitializer

	globals map[string]starlark.StringDict

	mainFile string
	mainFun  *starlark.Function

	schemaFile string
	schema     *schema.Schema
	schemaJSON []byte
}

func WithModuleLoader(loader ModuleLoader) AppletOption {
	return func(a *Applet) error {
		a.loader = loader
		return nil
	}
}

func WithSecretDecryptionKey(key *SecretDecryptionKey) AppletOption {
	return func(a *Applet) error {
		if decrypter, err := key.decrypterForApp(a); err != nil {
			return fmt.Errorf("preparing secret key: %w", err)
		} else {
			a.initializers = append(a.initializers, func(t *starlark.Thread) *starlark.Thread {
				decrypter.attachToThread(t)
				return t
			})
			return nil
		}
	}
}

func WithPrintFunc(print PrintFunc) AppletOption {
	return func(a *Applet) error {
		a.initializers = append(a.initializers, func(t *starlark.Thread) *starlark.Thread {
			t.Print = print
			return t
		})
		return nil
	}
}

func WithPrintDisabled() AppletOption {
	return WithPrintFunc(func(thread *starlark.Thread, msg string) {})
}

func NewApplet(id string, src []byte, opts ...AppletOption) (*Applet, error) {
	fn := id
	if !strings.HasSuffix(fn, ".star") {
		fn += ".star"
	}

	vfs := fstest.MapFS{
		fn: &fstest.MapFile{
			Data: src,
		},
	}

	return NewAppletFromFS(id, vfs, opts...)
}

func NewAppletFromFS(id string, fsys fs.FS, opts ...AppletOption) (*Applet, error) {
	a := &Applet{
		ID:      id,
		globals: make(map[string]starlark.StringDict),
	}

	for _, opt := range opts {
		if err := opt(a); err != nil {
			return nil, err
		}
	}

	if err := a.load(fsys); err != nil {
		return nil, err
	}

	return a, nil
}

// Run executes the applet's main function. It returns the render roots that are
// returned by the applet.
func (a *Applet) Run(ctx context.Context) (roots []render.Root, err error) {
	return a.RunWithConfig(ctx, nil)
}

// RunWithConfig exceutes the applet's main function, passing it configuration as a
// starlark dict. It returns the render roots that are returned by the applet.
func (a *Applet) RunWithConfig(ctx context.Context, config map[string]string) (roots []render.Root, err error) {
	var args starlark.Tuple
	if a.mainFun.NumParams() > 0 {
		starlarkConfig := AppletConfig(config)
		args = starlark.Tuple{starlarkConfig}
	}

	returnValue, err := a.Call(ctx, a.mainFun, args...)
	if err != nil {
		return nil, err
	}

	if returnRoot, ok := returnValue.(render_runtime.Rootable); ok {
		roots = []render.Root{returnRoot.AsRenderRoot()}
	} else if returnList, ok := returnValue.(*starlark.List); ok {
		roots = make([]render.Root, returnList.Len())
		iter := returnList.Iterate()
		defer iter.Done()
		i := 0
		var listVal starlark.Value
		for iter.Next(&listVal) {
			if listValRoot, ok := listVal.(render_runtime.Rootable); ok {
				roots[i] = listValRoot.AsRenderRoot()
			} else {
				return nil, fmt.Errorf(
					"expected app implementation to return Root(s) but found: %s (at index %d)",
					listVal.Type(),
					i,
				)
			}
			i++
		}
	} else {
		return nil, fmt.Errorf("expected app implementation to return Root(s) but found: %s", returnValue.Type())
	}

	return roots, nil
}

// CallSchemaHandler calls a schema handler, passing it a single
// string parameter and returning a single string value.
func (app *Applet) CallSchemaHandler(ctx context.Context, handlerName, parameter string) (result string, err error) {
	handler, found := app.schema.Handlers[handlerName]
	if !found {
		return "", fmt.Errorf("no exported handler named '%s'", handlerName)
	}

	resultVal, err := app.Call(
		ctx,
		handler.Function,
		starlark.String(parameter),
	)
	if err != nil {
		return "", fmt.Errorf("calling schema handler %s: %v", handlerName, err)
	}

	switch handler.ReturnType {
	case schema.ReturnOptions:
		options, err := schema.EncodeOptions(resultVal)
		if err != nil {
			return "", err
		}
		return options, nil

	case schema.ReturnSchema:
		sch, err := schema.FromStarlark(resultVal, app.globals[app.schemaFile])
		if err != nil {
			return "", err
		}

		s, err := json.Marshal(sch)
		if err != nil {
			return "", fmt.Errorf("serializing schema to JSON: %w", err)
		}

		return string(s), nil

	case schema.ReturnString:
		str, ok := starlark.AsString(resultVal)
		if !ok {
			return "", fmt.Errorf(
				"expected %s to return a string or string-like value",
				handler.Function.Name(),
			)
		}
		return str, nil
	}

	return "", fmt.Errorf("a very unexpected error happened for handler \"%s\"", handlerName)
}

// GetSchema returns the config for the applet.
func (app *Applet) GetSchema() string {
	return string(app.schemaJSON)
}

// RunTests runs all test functions that are defined in the applet source.
func (app *Applet) RunTests(t *testing.T) {
	app.initializers = append(app.initializers, func(thread *starlark.Thread) *starlark.Thread {
		starlarktest.SetReporter(thread, t)
		return thread
	})

	for file, globals := range app.globals {
		for name, global := range globals {
			if !strings.HasPrefix(name, "test_") {
				continue
			}

			if fun, ok := global.(*starlark.Function); ok {
				t.Run(fmt.Sprintf("%s/%s", file, name), func(t *testing.T) {
					if _, err := app.Call(context.Background(), fun); err != nil {
						t.Error(err)
					}
				})
			}
		}
	}
}

// Calls any callable from Applet.Globals. Pass args and receive a
// starlark Value, or an error if you're unlucky.
func (a *Applet) Call(ctx context.Context, callable *starlark.Function, args ...starlark.Value) (val starlark.Value, err error) {
	defer func() {
		if r := recover(); r != nil {
			err = fmt.Errorf("panic while running %s: %v", a.ID, r)
		}
	}()

	t := a.newThread(ctx)

	context.AfterFunc(ctx, func() {
		t.Cancel(context.Cause(ctx).Error())
	})

	resultVal, err := starlark.Call(t, callable, args, nil)
	if err != nil {
		evalErr, ok := err.(*starlark.EvalError)
		if ok {
			return nil, fmt.Errorf(evalErr.Backtrace())
		}
		return nil, fmt.Errorf(
			"in %s at %s: %s",
			callable.Name(),
			callable.Position().String(),
			err,
		)
	}

	return resultVal, nil
}

func (a *Applet) load(fsys fs.FS) (err error) {
	if err := fs.WalkDir(fsys, ".", func(path string, d fs.DirEntry, walkDirErr error) error {
		if walkDirErr != nil {
			return walkDirErr
		}

		if d.IsDir() || filepath.Dir(path) != "." {
			// only process files in the root directory
			return nil
		}

		if !strings.HasSuffix(path, ".star") {
			// not a starlark file
			return nil
		}

		return a.ensureLoaded(fsys, path)
	}); err != nil {
		return err
	}

	if a.mainFun == nil {
		return fmt.Errorf("no main() function found in %s", a.ID)
	}

	return nil
}

func (a *Applet) ensureLoaded(fsys fs.FS, path string, currentlyLoading ...string) (err error) {
	defer func() {
		if r := recover(); r != nil {
			err = fmt.Errorf("panic while executing %s: %v", a.ID, r)
		}
	}()

	// normalize path so that it can be used as a key
	path = filepath.Clean(path)
	if _, ok := a.globals[path]; ok {
		// already loaded, good to go
		return nil
	}

	// use the currentlyLoading slice to detect circular dependencies
	if slices.Contains(currentlyLoading, path) {
		return fmt.Errorf("circular dependency detected: %s -> %s", strings.Join(currentlyLoading, " -> "), path)
	} else {
		// mark this file as currently loading. if we encounter it again,
		// we have a circular dependency.
		currentlyLoading = append(currentlyLoading, path)
	}

	src, err := fs.ReadFile(fsys, path)
	if err != nil {
		return fmt.Errorf("reading %s: %v", path, err)
	}

	predeclared := starlark.StringDict{
		"struct": starlark.NewBuiltin("struct", starlarkstruct.Make),
	}

	thread := a.newThread(context.Background())

	// override loader to allow loading starlark files
	thread.Load = func(thread *starlark.Thread, module string) (starlark.StringDict, error) {
		// normalize module path
		modulePath := filepath.Clean(module)

		// if the module exists on the filesystem, load it
		if _, err := fs.Stat(fsys, modulePath); err == nil {
			// ensure the module is loaded, and pass the currentlyLoading slice
			// to detect circular dependencies
			if err := a.ensureLoaded(fsys, modulePath, currentlyLoading...); err != nil {
				return nil, err
			}

			if g, ok := a.globals[modulePath]; !ok {
				return nil, fmt.Errorf("module %s not loaded", modulePath)
			} else {
				return g, nil
			}
		}

		// fallback to default loader
		return a.loadModule(thread, module)
	}

	globals, err := starlark.ExecFileOptions(
		&syntax.FileOptions{
			Set:       true,
			Recursion: true,
		},
		thread,
		a.ID,
		src,
		predeclared,
	)
	if err != nil {
		return fmt.Errorf("starlark.ExecFile: %v", err)
	}
	a.globals[path] = globals

	// if the file is in the root directory, check for the main function
	// and schema function
	mainFun, _ := globals["main"].(*starlark.Function)
	if mainFun != nil {
		if a.mainFile != "" {
			return fmt.Errorf("multiple files with a main() function:\n- %s\n- %s", path, a.mainFile)
		}

		a.mainFile = path
		a.mainFun = mainFun
	}

	schemaFun, _ := globals[schema.SchemaFunctionName].(*starlark.Function)
	if schemaFun != nil {
		if a.schemaFile != "" {
			return fmt.Errorf("multiple files with a %s() function:\n- %s\n- %s", schema.SchemaFunctionName, path, a.schemaFile)
		}
		a.schemaFile = path

		schemaVal, err := a.Call(context.Background(), schemaFun)
		if err != nil {
			return fmt.Errorf("calling schema function for %s: %w", a.ID, err)
		}

		a.schema, err = schema.FromStarlark(schemaVal, globals)
		if err != nil {
			return fmt.Errorf("parsing schema for %s: %w", a.ID, err)
		}

		a.schemaJSON, err = json.Marshal(a.schema)
		if err != nil {
			return fmt.Errorf("serializing schema to JSON for %s: %w", a.ID, err)
		}
	}

	return nil
}

func (a *Applet) newThread(ctx context.Context) *starlark.Thread {
	t := &starlark.Thread{
		Name: a.ID,
		Load: a.loadModule,
		Print: func(thread *starlark.Thread, msg string) {
			fmt.Printf("[%s] %s\n", a.ID, msg)
		},
	}

	starlarkutil.AttachThreadContext(ctx, t)
	random.AttachToThread(t)

	for _, init := range a.initializers {
		t = init(t)
	}

	return t
}

func (a *Applet) loadModule(thread *starlark.Thread, module string) (starlark.StringDict, error) {
	if a.loader != nil {
		mod, err := a.loader(thread, module)
		if err == nil {
			return mod, nil
		}
	}

	switch module {
	case "render.star":
		return render_runtime.LoadRenderModule()

	case "animation.star":
		return animation_runtime.LoadAnimationModule()

	case "schema.star":
		return schema.LoadModule()

	case "cache.star":
		return LoadCacheModule()

	case "secret.star":
		return LoadSecretModule()

	case "xpath.star":
		return xpath.LoadXPathModule()

	case "compress/gzip.star":
		return starlark.StringDict{
			starlibgzip.Module.Name: starlibgzip.Module,
		}, nil

	case "compress/zipfile.star":
		// Starlib expects you to load the ZipFile function directly, rather than having it be part of a namespace.
		// Wraps this to be more consistent with other pixlet modules, as follows:
		//   load("compress/zipfile.star", "zipfile")
		//   archive = zipfile.ZipFile("/tmp/foo.zip")
		m, _ := starlibzip.LoadModule()
		return starlark.StringDict{
			"zipfile": &starlarkstruct.Module{
				Name:    "zipfile",
				Members: m,
			},
		}, nil

	case "encoding/base64.star":
		return starlibbase64.LoadModule()

	case "encoding/csv.star":
		return starlibcsv.LoadModule()

	case "encoding/json.star":
		return starlark.StringDict{
			starlibjson.Module.Name: starlibjson.Module,
		}, nil

	case "hash.star":
		return starlibhash.LoadModule()

	case "hmac.star":
		return hmac.LoadModule()

	case "http.star":
		return starlarkhttp.LoadModule()

	case "html.star":
		return starlibhtml.LoadModule()

	case "humanize.star":
		return humanize.LoadModule()

	case "math.star":
		return starlark.StringDict{
			starlibmath.Module.Name: starlibmath.Module,
		}, nil

	case "re.star":
		return starlibre.LoadModule()

	case "sunrise.star":
		return sunrise.LoadModule()

	case "time.star":
		return starlark.StringDict{
			starlibtime.Module.Name: starlibtime.Module,
		}, nil

	case "random.star":
		return random.LoadModule()

	case "qrcode.star":
		return qrcode.LoadModule()

	case "assert.star":
		return starlarktest.LoadAssertModule()

	default:
		return nil, fmt.Errorf("invalid module: %s", module)
	}
}
