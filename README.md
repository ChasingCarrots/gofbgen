# gofbgen
A Go package to automatically generate flatbuffer serialization functions for Go structs.

This package is intended to bridge a gap in the usual flatbuffer workflow: You have defined your flatbuffers, generated code for reading them, but still need to manually write them. This is error-prone and generally tedious. Couldn't the computer do that for you? As it turns out, the answer is a resounding yes! With this package, you can simply annotate your structs with information about their flatbuffer layout, run some code generation and then get the serialization code for free.

The package is implemented as a tag handler for the tag processor from the [gotransform package](https://github.com/chasingcarrots/gotransform).

## Annotating a struct
Assume that you have the following flatbuffer definition:
```flatbuffer
struct Vector2 {
    x:float;
    y:float;
}

table MoveMessage {
    startTime:float;
    position:Vector2;
    velocity:Vector2;
}
```
They are compiled into a go package that we will call `github.com/chasingcarrots/flatbuffertest` for illustration purposes. Your Go-struct that you would like to serialize might live in the package `github.com/chasingcarrots/movement` and look like this:
```golang
package movement

import "github.com/chasingcarrots/gofbgen/tags"

type Vector2 struct {
    x, y float32
}

type Move struct {
    // Use a tag to mark this struct as requiring serialization.
    // The Type field points to the full path of the struct generated by the flatbuffer tool
    // The Func field holds the name of the function to generate. If omitted, its default name
    // is Serialize<Type>.
    tags.Flatbuffer `Type:"github.com/chasingcarrots/flatbuffertest/MoveMessage" Func:"SerializeMove"`
    // when did the move start?
    time float32 `fbName:"startTime"`
    // where did we start and how fast are we going?
    position Vector2 `fbName:"position"`
    velocity Vector2 `fbName:"velocity"`
}
```
The tag handler for flatbuffers knows how to handle `float32` valued fields, but it needs some help with the `Vector2` fields. We'll see in a minute how this works, but this is the code that the code generator will spit out in the (modulo comments):

```golang
package movement

import (
	"github.com/chasingcarrots/flatbuffertest"
	flatbuffers "github.com/google/flatbuffers/go" // this import is added automatically
)

func SerializeMove(flatbuilder *flatbuffers.Builder, target *MoveMessage) flatbuffers.UOffsetT {
	flatbuffertest.MoveMessageStart(flatbuilder)
	flatbuffertest.MoveMessageAddStarttime(flatbuilder, target.time)
	flatbuffertest.MoveMessageAddPosition(flatbuilder, flatbuffertest.CreateVector2(flatbuilder, target.position.X, target.position.Y))
	flatbuffertest.MoveMessageAddVelocity(flatbuilder, flatbuffertest.CreateVector2(flatbuilder, target.velocity.X, target.velocity.Y))
	return flatbuffertest.AccountStateEnd(flatbuilder)
}
```

### Running the Code Generation
The code generator is implemented as a tag handler for the tag processor from the [gotransform package](https://github.com/chasingcarrots/gotransform). You can read all about setting up the tag handler over at that package's [github page](https://github.com/chasingcarrots/gotransform). Here is one way to create the handler:

```golang
func setupFlatbufferTask(outputPath string) *newHandlers.FlatbufferHandler {
	// add serialization hints for primitive types that are commonly used
	hints := map[string]func(string) string{
		// specify how to handle a vector
		"github.com/chasingcarrots/movement/Vector2": func(x string) string {
			return "flatbuffertest.CreateVector2(flatbuilder, " + x + ".X, " + x + ".Y)"
		},
	}
	// the package that the serialization code should live in
	pkg := "movement"
	// you have to specify the imports manually right now, the tool doesn't currently 
	// collect them automatically.
	imports := []string{"github.com/chasingcarrots/flatbuffertest"}
	return gofbgen.New(outputPath, pkg, imports, hints)
}
```
This will generate the code from above when used with the infrastructure from the gotransform package. Note the use of the `hints` map to specify how the `Vector2` struct is serialized: It takes an identifier `x` that is of type `Vector2` and constructs the serialization code for it. I know, this looks really cheap, but it does the job.

### Tagging Fields for Flatbuffer Generation
Fields of a struct can be tagged in a variety of ways to get different effects. There are three different tags available:
 * `fbName:"NAME"` is a necessary tag. Each field that needs to be serialized into a flatbuffers *needs* to have this field. It specifies what the name of the corresponding field in the flatbuffer is.
 * `fbType:"TYPE"` is an optional tag that gives more context as to how this field should be serialized (see below). You do not need to usually specify this; the tool understands how to serialize primitive types and their vectorized versions.
 * `fbFunc:"FUNCNAME"` is an optional tag that specifies a function that should be used for serialization. This function should have the type `func(*flatbuffer.Builder, *TYPE) flatbuffers.UOffsetT` where `TYPE` is the type of the field that you are annotating (see below for details).
 
The different types (and their effects) used for `fbType` are as follows:
 * `uint, uint8, uint16, uint32, uint64, int, int8, int16, int32, int64, byte, float32, float64` -- numeric primitive types. Using these will add a cast to the specific type. For example, assume you have the following definition:
 ```golang
 type TestID uint16
 type Test struct {
	ID TestID `fbName:"ID" fbType:"uint16"`
 }
 ```
 Here, we added the `fbType:"uin16"` annotation to communicate that the field should be cast to `uint16` prior to serialization.
 * `bool` -- booleans. While flatbuffer may contain booleans, Go only allows them to be set from a `byte` value. As such, a field of boolean type or with `fbType:"bool"` is serialized by branching and computing its byte value prior to serialization.
 * `string` -- you should never set this on anything. Strings automatically have this as their `fbType` and are serialized by using `flatbuilder.CreateString`. The tool makes sure that all strings are serialized prior to the struct itself.
 * `table` -- specifies that this field has reference semantics, i.e. it needs to be serialized separately and the buffer itself can only hold the offset of this serialized version. If this is set, you need to set a serialization function using `fbType:"FUNCNAME"`, see the next point for details.
 * `custom` -- allows you to do custom serialization. A field with this `fbType` is required to also specify a function via `fbFunc:"FUNCNAME"` for serialization. This function must have the signature `func(*flatbuffer.Builder, *TYPE) flatbuffers.UOffsetT` where `TYPE` is the type of the field that you are annotating. A call to this function will be inserted to serialize this member before serializing the rest of the struct to the flatbuffer. This is usually necessary whenever you want to serialize a struct that contains a `map`.

### Slices and maps as Vectors
The tool is aware of slices and strings and automatically serializes them properly. The meaning of the `fbType` annotation changes slightly: `custom` still means that your function is called. `table` means that each element in the vector has reference semantics; each entry of the slice is serialized with the function you specified, the references are collected, and added to the vector. The same remarks hold for maps, with the added fineprint that keys of maps are ignored -- only the maps in the value are serialized. If you need behavior that differs from this, use the `custom` annotation to provide your own custom serialization logic.

### Overview
As a reminder, here is how serialization happens for different type/function combinations. In the table below, *value* means that the field is serialized as a flatbuffer struct (in-place, by value) whereas *reference* means the field will be serialized prior to serializing the rest of the struct and only a reference will be inserted.

| fbType \ fbFunc | specified                      | not specified              |
| :---            | :---                           | :---                       |
| not specified   | value, calls function in-place | looks for default function |
| 'table'         | reference, calls function      | illegal                    |
| 'custom'        | reference, calls function      | illegal                    |
| primitive type  | illegal                        | casts to type              |


For vectors, the table looks like this:

| fbType \ fbFunc | specified                                       | not specified              |
| :---            | :---                                            | :---                       |
| not specified   | value, calls function in-place for each entry   | looks for default function, calls it for every entry |
| 'table'         | reference, calls function for each entry        | illegal                    |
| 'custom'        | reference, calls function once for whole vector | illegal                    |
| primitive type  | illegal                                         | casts every entry to type  |


## Limitations
The code in this package can only be used when one of your flatbuffers can be generated from a single struct.

