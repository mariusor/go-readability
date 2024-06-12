package readability

import (
	"bytes"
	"encoding/base64"
	"fmt"
	"io"
	"log"
	"log/slog"
	"math"
	"net/http"
	"regexp"
	"strconv"
	"strings"

	"github.com/PuerkitoBio/goquery"
	"golang.org/x/net/html"
)

var (
	Logger = slog.New(
		slog.NewTextHandler(io.Discard, &slog.HandlerOptions{}).WithGroup("[readability]"),
	)

	replaceBrsRegexp   = regexp.MustCompile(`(?i)(<br[^>]*>[ \n\r\t]*){2,}`)
	replaceFontsRegexp = regexp.MustCompile(`(?i)<(\/?)\s*font[^>]*?>`)

	blacklistCandidatesRegexp  = regexp.MustCompile(`(?i)popupbody`)
	okMaybeItsACandidateRegexp = regexp.MustCompile(`(?i)and|article|body|column|main|shadow`)
	unlikelyCandidatesRegexp   = regexp.MustCompile(`(?i)combx|comment|community|hidden|disqus|modal|extra|foot|header|menu|remark|rss|shoutbox|sidebar|sponsor|ad-break|agegate|pagination|pager|popup|share`)
	divToPElementsRegexp       = regexp.MustCompile(`(?i)<(dl|div|ol|pre|table|ul|header|footer|article)`)

	okMaybeItsAHeaderFooterRegexp = regexp.MustCompile(`(?i)(header|footer|h1|h2|h3|h4|h5|h6)`)

	negativeRegexp = regexp.MustCompile(`(?i)combx|comment|com-|foot|footer|footnote|masthead|media|meta|outbrain|promo|related|scroll|shoutbox|sidebar|sponsor|shopping|tags|tool|widget`)
	positiveRegexp = regexp.MustCompile(`(?i)article|body|content|entry|hentry|main|page|pagination|post|text|blog|story`)

	stripCommentRegexp = regexp.MustCompile(`(?s)\<\!\-{2}.+?-{2}\>`)

	sentenceRegexp    = regexp.MustCompile(`\.( |$)`)
	unlikelySentences = regexp.MustCompile(`(?i)previous|next|published|bookmark|permalink`)

	normalizeWhitespaceRegexp     = regexp.MustCompile(`[\t ]+`)
	normalizeEOLRegexp            = regexp.MustCompile(`[\r\n\f]+`)
	normalizeHtmlWhiteSpaceRegexp = regexp.MustCompile(`(&nbsp;)+`)
)

type candidate struct {
	selection *goquery.Selection
	score     float32
}

func (c *candidate) Node() *html.Node {
	return c.selection.Get(0)
}

type Document struct {
	input         string
	document      *goquery.Document
	content       string
	candidates    map[*html.Node]*candidate
	bestCandidate *candidate

	Title                    string
	EnsureTitleInArticle     bool
	RemoveUnlikelyCandidates bool
	WeightClasses            bool
	CleanConditionally       bool
	BestCandidateHasImage    bool
	RetryLength              int
	MinTextLength            int
	RemoveEmptyNodes         bool
	WhitelistTags            []string
	WhitelistAttrs           map[string][]string
}

func NewDocument(s string) (*Document, error) {
	d := &Document{
		input:                    s,
		WhitelistTags:            []string{"div", "p"},
		WhitelistAttrs:           make(map[string][]string),
		RemoveUnlikelyCandidates: true,
		WeightClasses:            true,
		CleanConditionally:       true,
		RetryLength:              250,
		MinTextLength:            25,
		RemoveEmptyNodes:         true,
	}
	err := d.initializeHtml(s)
	if err != nil {
		return nil, err
	}

	return d, nil
}

func (d *Document) initializeHtml(s string) error {
	// replace consecutive <br>'s with p tags
	s = replaceBrsRegexp.ReplaceAllString(s, "</p><p>")

	// replace font tags
	s = replaceFontsRegexp.ReplaceAllString(s, `<${1}span>`)

	// manually strip regexps since html parser seems to miss some
	s = stripCommentRegexp.ReplaceAllString(s, "")

	doc, err := goquery.NewDocumentFromReader(strings.NewReader(s))
	if err != nil {
		return err
	}

	// if no body (like from a redirect or empty string)
	if doc.Find("body").Length() == 0 {
		s = "<body/>"
		return d.initializeHtml(s)
	}

	d.document = doc
	return nil
}

func (d *Document) ContentWithHTML() (content string, html string) {
	if d.content == "" {
		d.prepareCandidates()

		article := d.getArticle()
		articleText := d.sanitize(article)

		length := len(articleText)
		if length < d.RetryLength {
			retry := true

			if d.RemoveUnlikelyCandidates {
				d.RemoveUnlikelyCandidates = false
			} else if d.WeightClasses {
				d.WeightClasses = false
			} else if d.CleanConditionally {
				d.CleanConditionally = false
			} else {
				d.content = articleText
				retry = false
			}

			if retry {
				Logger.With(
					slog.Int("length", length),
					slog.Int("retry", d.RetryLength),
				).Debug("Retrying due to length smaller than retry")
				_ = d.initializeHtml(d.input)
				articleText, _ = d.ContentWithHTML()
			}
		}
		d.content = articleText
	}

	return d.content, d.getArticle()
}

func (d *Document) Content() (content string) {
	content, _ = d.ContentWithHTML()
	return content
}

var (
	// noscript might be valid, but probably not so we'll just remove it
	noContentTags []string = []string{"script", "style", "noscript", "link"}
)

func (d *Document) prepareCandidates() {
	d.document.Find(strings.Join(noContentTags, ",")).Each(func(i int, s *goquery.Selection) {
		Logger.With(slog.String("tag", getName(s))).Debug("Removing no content tag")
		removeNodes(s)
	})

	if d.RemoveUnlikelyCandidates {
		d.removeUnlikelyCandidates()
	}

	d.transformMisusedDivsIntoParagraphs()
	d.scoreParagraphs(d.MinTextLength)
	d.selectBestCandidate()
}

func (d *Document) selectBestCandidate() {
	var best *candidate

	for _, c := range d.candidates {
		if best == nil {
			best = c
		} else if best.score < c.score {
			best = c
		}
	}

	if best == nil {
		best = &candidate{d.document.Find("body"), 0}
	}

	d.bestCandidate = best
}

func (d *Document) getTitle() string {
	return strings.TrimSpace(d.document.Find("head title").First().Text())
}

func (d *Document) getArticle() string {
	output := new(bytes.Buffer)

	siblingScoreThreshold := float32(math.Max(float64(10), float64(d.bestCandidate.score*.2)))

	d.bestCandidate.selection.Siblings().Union(d.bestCandidate.selection).Each(func(i int, s *goquery.Selection) {
		append := false
		n := s.Get(0)

		if n == d.bestCandidate.Node() {
			append = true
		} else if c, ok := d.candidates[n]; ok && c.score >= siblingScoreThreshold {
			append = true
		}

		if s.Is("p") {
			linkDensity := d.getLinkDensity(s)
			content := strings.TrimSpace(s.Text())
			contentLength := len(content)
			if contentLength == 0 {
				return
			}

			if contentLength >= 80 && linkDensity < .25 {
				append = true
			} else if contentLength < 80 && linkDensity == 0 {
				append = sentenceRegexp.MatchString(content)
			}
		}

		if append {
			tag := "div"
			if s.Is("p") {
				tag = n.Data
			}

			html, _ := s.Html()
			html = strings.TrimSpace(html)
			if len(html) == 0 {
				return
			}

			_, _ = fmt.Fprintf(output, "<%s>%s</%s>\n", tag, html, tag)
		}
	})

	return output.String()
}

func (d *Document) removeUnlikelyCandidates() {
	d.document.Find("*").Not("html,title,body").Each(func(i int, s *goquery.Selection) {
		class, _ := s.Attr("class")
		id, _ := s.Attr("id")

		str := class + id

		if blacklistCandidatesRegexp.MatchString(str) || (unlikelyCandidatesRegexp.MatchString(str) && !okMaybeItsACandidateRegexp.MatchString(str) && !okMaybeItsAHeaderFooterRegexp.MatchString(goquery.NodeName(s))) {
			Logger.With(slog.String("tag", getName(s))).Debug("Removing unlikely candidate")
			removeNodes(s)
		}
	})
}

func (d *Document) transformMisusedDivsIntoParagraphs() {
	d.document.Find("header,footer,article,div").Each(func(i int, s *goquery.Selection) {
		html, err := s.Html()
		if err != nil {
			Logger.With(slog.String("error", err.Error())).Warn("Unable to transform 'div' to 'p'")
			return
		}

		// transform <div>s that do not contain other block elements into <p>s
		if !divToPElementsRegexp.MatchString(html) {
			Logger.With(slog.String("tag", getName(s))).Debug("Altering tag to 'p'")

			node := s.Get(0)
			node.Data = "p"
		}
	})
}

func (d *Document) scoreParagraphs(minimumTextLength int) {
	candidates := make(map[*html.Node]*candidate)

	d.document.Find("p,td").Each(func(i int, s *goquery.Selection) {
		text := strings.TrimSpace(s.Text())

		// if this paragraph is less than x chars, don't count it
		if len(text) < minimumTextLength {
			return
		}

		parent := s.Parent()
		parentNode := parent.Get(0)

		grandparent := parent.Parent()
		var grandparentNode *html.Node
		if grandparent.Length() > 0 {
			grandparentNode = grandparent.Get(0)
		}

		if _, ok := candidates[parentNode]; !ok {
			candidates[parentNode] = d.scoreNode(parent)
		}
		if grandparentNode != nil {
			if _, ok := candidates[grandparentNode]; !ok {
				candidates[grandparentNode] = d.scoreNode(grandparent)
			}
		}

		contentScore := float32(1.0)
		contentScore += float32(strings.Count(text, ",") + 1)
		contentScore += float32(math.Min(float64(int(len(text)/100.0)), 3))

		candidates[parentNode].score += contentScore
		if grandparentNode != nil {
			candidates[grandparentNode].score += contentScore / 2.0
		}
	})

	// scale the final candidates score based on link density. Good content
	// should have a relatively small link density (5% or less) and be mostly
	// unaffected by this operation
	for _, candidate := range candidates {
		candidate.score = candidate.score * (1 - d.getLinkDensity(candidate.selection))
	}

	d.candidates = candidates
}

func (d *Document) getLinkDensity(s *goquery.Selection) float32 {
	linkLength := len(s.Find("a").Text())
	textLength := len(s.Text())

	if textLength == 0 {
		return 0
	}

	return float32(linkLength) / float32(textLength)
}

func (d *Document) classWeight(s *goquery.Selection) int {
	weight := 0
	if !d.WeightClasses {
		return weight
	}

	class, _ := s.Attr("class")
	id, _ := s.Attr("id")

	if class != "" {
		if negativeRegexp.MatchString(class) && !okMaybeItsAHeaderFooterRegexp.MatchString(goquery.NodeName(s)) {
			weight -= 25
		}

		if positiveRegexp.MatchString(class) {
			weight += 25
		}
	}

	if id != "" {
		if negativeRegexp.MatchString(id) && !okMaybeItsAHeaderFooterRegexp.MatchString(goquery.NodeName(s)) {
			weight -= 25
		}

		if positiveRegexp.MatchString(id) {
			weight += 25
		}
	}

	return weight
}

func (d *Document) scoreNode(s *goquery.Selection) *candidate {
	contentScore := d.classWeight(s)
	if s.Is("div") {
		contentScore += 5
	} else if s.Is("blockquote,form,fieldset") {
		contentScore = 3
	} else if s.Is("th") {
		contentScore -= 5
	}

	return &candidate{s, float32(contentScore)}
}

var (
	headerTags = []string{"h1", "h2", "h3", "h4", "h5", "h6"}
)

func (d *Document) sanitize(article string) string {
	doc, err := goquery.NewDocumentFromReader(strings.NewReader(article))
	d.Title = d.getTitle()
	if err != nil {
		Logger.With(slog.String("error", err.Error())).Warn("Unable to create document")
		return ""
	}

	s := doc.Find("body")
	s.Find(strings.Join(headerTags, ",")).Each(func(i int, header *goquery.Selection) {
		if d.classWeight(header) < 0 || d.getLinkDensity(header) > 0.33 {
			Logger.With(slog.String("tag", getName(header))).Debug("Removing tag")
			removeNodes(header)
		}
	})

	s.Find("input,select,textarea,button,object,iframe,embed").Each(func(i int, s *goquery.Selection) {
		Logger.With(slog.String("tag", getName(s))).Debug("Removing non-content tag")
		removeNodes(s)
	})

	//d.cleanConditionally(s, "p")

	if d.RemoveEmptyNodes {
		s.Find("p").Each(func(i int, s *goquery.Selection) {
			html, _ := s.Html()
			if len(strings.TrimSpace(html)) == 0 {
				Logger.With(slog.String("tag", getName(s))).Debug("Removing empty node")
				removeNodes(s)
			}
		})
	}

	d.cleanConditionally(s, "table,ul,div")

	// we'll sanitize all elements using a whitelist
	replaceWithWhitespace := map[string]bool{
		"br":         true,
		"hr":         true,
		"h1":         true,
		"h2":         true,
		"h3":         true,
		"h4":         true,
		"h5":         true,
		"h6":         true,
		"dl":         true,
		"dd":         true,
		"ol":         true,
		"li":         true,
		"ul":         true,
		"address":    true,
		"blockquote": true,
		"center":     true,
	}

	whitelist := make(map[string]bool)
	for _, tag := range d.WhitelistTags {
		tag = strings.ToLower(tag)
		whitelist[tag] = true
		delete(replaceWithWhitespace, tag)
	}

	if d.EnsureTitleInArticle {
		top := doc.Find("h1").First()
		missingTitle := top.Length() == 0
		maybeMissingTitle, maybeTitle := topLineMaybeIsTitle(top, d.Title)

		if missingTitle {
			top = doc.Find("p").First()
		}
		if missingTitle || maybeMissingTitle {
			titText := &html.Node{
				Type: html.TextNode,
				Data: d.Title,
			}
			h1Title := &html.Node{
				Type: html.ElementNode,
				Data: "h1",
			}
			h1Title.AppendChild(titText)
			if maybeMissingTitle {
				html, err := top.Html()
				if err != nil {
					log.Print(err.Error())
				}
				top.SetHtml(strings.Replace(html, maybeTitle, "", 1))
				top.ReplaceWithNodes(h1Title)
			} else {
				top.PrependNodes(h1Title)
			}
		}
	}

	var text string

	s.Find("*").Each(func(i int, s *goquery.Selection) {
		if text != "" {
			return
		}

		// only look at element nodes
		node := s.Get(0)
		if node.Type != html.ElementNode {
			return
		}

		// if element is in whitelist then delete all unwanted attributes
		if _, ok := whitelist[node.Data]; ok {
			var allowedAttrs []html.Attribute
			if whiteAttrs, ok := d.WhitelistAttrs[node.Data]; ok {
				for _, el := range node.Attr {
					attrFound := false
					for _, w := range whiteAttrs {
						if w == el.Key {
							attrFound = true
							break
						}
					}
					if attrFound {
						allowedAttrs = append(allowedAttrs, el)
					}
				}
			}

			node.Attr = allowedAttrs
		} else {
			if _, ok := replaceWithWhitespace[node.Data]; ok {
				// just replace with a text node and add whitespace
				node.Data = fmt.Sprintf(" %s ", s.Text())
				node.Type = html.TextNode
				node.FirstChild = nil
				node.LastChild = nil
			} else {
				if node.Parent == nil {
					text = s.Text()
					return
				} else {
					// replace node with children
					replaceNodeWithChildren(node)
				}
			}
		}
	})

	addTitle(d.Title, doc)

	// download images and embed them using base64 encoding
	s.Find("img").Each(func(i int, s *goquery.Selection) {
		node := s.Get(0)
		if node.Type != html.ElementNode {
			return
		}

		for i, attr := range node.Attr {
			if attr.Key != "src" {
				continue
			}
			attr.Val = fetchImageAsDataURL(attr.Val)
			node.Attr[i] = attr
		}
	})

	if text == "" {
		text, _ = doc.Html()
	}

	return sanitizeWhitespace(text)
}

func addTitle(title string, doc *goquery.Document) {
	titText := &html.Node{
		Type: html.TextNode,
		Data: title,
	}
	titNode := &html.Node{
		Type: html.ElementNode,
		Data: "title",
	}
	titNode.AppendChild(titText)
	doc.Find("head").AppendNodes(titNode)
}

func (d *Document) cleanConditionally(s *goquery.Selection, selector string) {
	if !d.CleanConditionally {
		return
	}

	s.Find(selector).Each(func(i int, s *goquery.Selection) {
		node := s.Get(0)
		weight := float32(d.classWeight(s))
		contentScore := float32(0)

		if c, ok := d.candidates[node]; ok {
			contentScore = c.score
		}

		if weight+contentScore < 0 {
			Logger.With(
				slog.String("text", s.Text()),
				slog.String("tag", getName(s)),
				slog.String("node", node.Data),
				slog.Float64("weight", float64(weight)),
				slog.Float64("score", float64(contentScore)),
			).Debug("Conditionally cleaned node due to weight and content score")
			removeNodes(s)
			return
		}

		text := s.Text()
		if strings.Count(text, ",") < 10 {
			counts := map[string]int{
				"p":     s.Find("p").Length(),
				"img":   s.Find("img").Length(),
				"li":    s.Find("li").Length() - 100,
				"a":     s.Find("a").Length(),
				"embed": s.Find("embed").Length(),
				"input": s.Find("input").Length(),
			}

			contentLength := len(strings.TrimSpace(text))
			linkDensity := d.getLinkDensity(s)
			remove := false
			reason := ""

			if counts["img"] > counts["p"] {
				reason = "too many images"
				remove = true
			} else if counts["li"] > counts["p"] && !s.Is("ul,ol") {
				reason = "more <li>s than <p>s"
				remove = true
			} else if counts["input"] > int(counts["p"]/3.0) {
				reason = "less than 3x <p>s than <input>s"
				remove = true
			} else if contentLength < d.MinTextLength && (counts["img"] == 0 || counts["img"] > 2) {
				reason = "too short content length without a single image"
				remove = true
			} else if weight < 25 && linkDensity > 0.2 {
				reason = fmt.Sprintf("too many links for its weight (%f)", weight)
				remove = true
			} else if weight >= 25 && linkDensity > 0.5 {
				reason = fmt.Sprintf("too many links for its weight (%f)", weight)
				remove = true
			} else if (counts["embed"] == 1 && contentLength < 75) || counts["embed"] > 1 {
				reason = "<embed>s with too short a content length, or too many <embed>s"
				remove = true
			}
			if unlikelySentences.MatchString(text) && contentLength < d.MinTextLength {
				reason = "short text contains unlikely words"
				remove = true
			}

			if remove {
				Logger.With(
					slog.String("text", s.Text()),
					slog.String("tag", getName(s)),
					slog.String("node", node.Data),
					slog.Float64("weight", float64(weight)),
					slog.Float64("score", float64(contentScore)),
					slog.String("reason", reason),
				).Debug("Conditionally cleaned node due to reason")
				removeNodes(s)
			}
		}
	})
}

func getName(s *goquery.Selection) string {
	class, _ := s.Attr("class")
	id, _ := s.Attr("id")
	idLbl, classLbl := "", ""
	if len(id) > 0 {
		idLbl = "#" + id
	}
	if len(class) > 0 {
		classLbl = "." + class
	}
	if s.Length() > 1 {
		return fmt.Sprintf("%s%s%s[%d]", goquery.NodeName(s), idLbl, classLbl, s.Length())
	} else {
		return fmt.Sprintf("%s%s%s", goquery.NodeName(s), idLbl, classLbl)
	}
}

func removeNodes(s *goquery.Selection) {
	s.Each(func(i int, s *goquery.Selection) {
		parent := s.Parent()
		if parent.Length() == 0 {
			// TODO???
		} else {
			parent.Get(0).RemoveChild(s.Get(0))
		}
	})
}

func replaceNodeWithChildren(n *html.Node) {
	var next *html.Node
	parent := n.Parent

	for c := n.FirstChild; c != nil; c = next {
		next = c.NextSibling
		n.RemoveChild(c)

		parent.InsertBefore(c, n)
	}

	parent.RemoveChild(n)
}

func sanitizeWhitespace(text string) string {
	text = normalizeHtmlWhiteSpaceRegexp.ReplaceAllString(text, " ")
	text = normalizeWhitespaceRegexp.ReplaceAllString(text, " ")
	text = normalizeEOLRegexp.ReplaceAllString(text, "\n")
	text = strings.TrimSpace(text)
	return text
}

func levenshtein(str1, str2 []rune) int {
	s1len := len(str1)
	s2len := len(str2)
	column := make([]int, len(str1)+1)

	for y := 1; y <= s1len; y++ {
		column[y] = y
	}
	for x := 1; x <= s2len; x++ {
		column[0] = x
		lastkey := x - 1
		for y := 1; y <= s1len; y++ {
			oldkey := column[y]
			var incr int
			if str1[y-1] != str2[x-1] {
				incr = 1
			}

			column[y] = minimum(column[y]+1, column[y-1]+1, lastkey+incr)
			lastkey = oldkey
		}
	}
	return column[s1len]
}

func minimum(a, b, c int) int {
	if a < b {
		if a < c {
			return a
		}
	} else {
		if b < c {
			return b
		}
	}
	return c
}

func topLineMaybeIsTitle(s *goquery.Selection, t string) (bool, string) {
	lines := strings.SplitN(s.Text(), "\n", 10)
	for _, line := range lines {
		lineLength := len(line)
		if lineLength == 0 {
			continue
		}
		if lineLength < len(t) {
			if levenshtein([]rune(line), []rune(t[:lineLength])) == 0 {
				return true, line
			}
		}
	}
	return false, ""
}

func fetchImageAsDataURL(uri string) string {
	r, err := http.Get(uri)
	if err != nil {
		Logger.With(slog.String("uri", uri)).Warn("unable to fetch image")
		return uri
	}
	if r.StatusCode != http.StatusOK {
		Logger.With(slog.String("uri", uri), slog.Int("status_code", r.StatusCode)).Warn("unable to fetch image")
		return uri
	}
	defer r.Body.Close()

	contentType := r.Header.Get("Content-Type")
	raw, err := io.ReadAll(r.Body)
	if err != nil {
		Logger.With(slog.String("uri", uri), slog.String("err", err.Error())).Warn("unable to read image data")
		return uri
	}
	contentLength := int64(len(raw))
	if cl := r.Header.Get("Content-Length"); len(cl) > 0 {
		contentLength, _ = strconv.ParseInt(cl, 10, 64)
	}
	if len(raw) != int(contentLength) {
		Logger.With(slog.String("uri", uri), slog.Int("blen", len(raw)), slog.Int64("Content-Length", contentLength)).Warn("unexpected byte length")
	}
	data := make([]byte, base64.URLEncoding.EncodedLen(len(raw)))
	base64.StdEncoding.Encode(data, raw)
	return fmt.Sprintf("data:%s;base64,%s", contentType, data)
}
