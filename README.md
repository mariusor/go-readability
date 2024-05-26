# go-readability

go-readability is library for extracting the main content off of an HTML page. This library implements the readability algorithm created by arc90 labs and was heavily inspired by https://github.com/cantino/ruby-readability.

This is a fork of [GoReadability from Mauidude](https://github.com/mariusor/go-readability). It contains some additions I required for my own usage.

## Installation

`go install github.com/mariusor/go-readability`

## CLI Tool

You can run readability via the command line to extract content from a single HTML file by running the following command:

```bash
$ readability path/to/file.html
```

For help with usage and options you can run the following:

```bash
$ readability --help
```

## Example

```
import(
  "github.com/mariusor/go-readability"
)

...

doc, err := readability.NewDocument(html)
if err != nil {
  // do something ...
}

content := doc.Content()
// do something with my content

```


## Tests

To run tests
`go test github.com/mariusor/go-readability`
