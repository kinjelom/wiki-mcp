package wiki

import "testing"

func TestHTMLToText(t *testing.T) {
	tests := []struct{ in, want string }{
		{`<p>Hello <strong>world</strong>!</p><p>Second</p>`, "Hello world!\n\nSecond"},
		{`<h2>Title</h2><ul><li>one</li><li>two <em>2</em></li></ul>`, "## Title\n\n- one\n- two 2"},
		{`<table><tr><th>A</th><th>B</th></tr><tr><td>1</td><td>2</td></tr></table>`, "A | B\n1 | 2"},
		{"<pre>a  b\n  c</pre><p>x</p>", "a  b\n  c\n\nx"},
		{`...the <strong class="search_hit">deploy</strong>ment of  BOSH...`, "...the deployment of BOSH..."},
		{`<script>alert(1)</script><p>ok<br>line</p>`, "ok\nline"},
		{`<p>a &amp; b &lt;c&gt;</p>`, "a & b <c>"},
	}
	for _, tt := range tests {
		if got := HTMLToText(tt.in); got != tt.want {
			t.Errorf("HTMLToText(%q) =\n%q\nwant\n%q", tt.in, got, tt.want)
		}
	}
}

func TestWindow(t *testing.T) {
	items := []int{1, 2, 3, 4, 5}
	if got, more := Window(items, 0, 2); len(got) != 2 || !more {
		t.Errorf("Window(0,2) = %v %v", got, more)
	}
	if got, more := Window(items, 3, 2); len(got) != 2 || more {
		t.Errorf("Window(3,2) = %v %v", got, more)
	}
	if got, more := Window(items, 9, 2); len(got) != 0 || more {
		t.Errorf("Window(9,2) = %v %v", got, more)
	}
}
