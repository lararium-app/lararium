import React, { useState } from "react";
import { Copy, Check } from "lucide-react";

interface MarkdownRendererProps {
  content: string;
  isStreaming?: boolean;
}

interface CodeBlockProps {
  language: string;
  code: string;
}

function CodeBlock({ language, code }: CodeBlockProps) {
  const [copied, setCopied] = useState(false);

  const handleCopy = () => {
    navigator.clipboard.writeText(code).then(() => {
      setCopied(true);
      setTimeout(() => setCopied(false), 1500);
    }).catch(() => {});
  };

  return (
    <div className="my-3 rounded-lg overflow-hidden border border-line bg-panel2">
      <div className="flex items-center justify-between px-3 py-1.5 bg-panel border-b border-line text-xs text-ink3 font-mono">
        <span>{language || "code"}</span>
        <button
          type="button"
          onClick={handleCopy}
          className="flex items-center gap-1 px-2 py-0.5 rounded hover:bg-line2 text-ink2 hover:text-ink transition-colors"
          title="Copy code"
        >
          {copied ? (
            <>
              <Check className="w-3.5 h-3.5 text-ok" />
              <span className="text-ok">Copied!</span>
            </>
          ) : (
            <>
              <Copy className="w-3.5 h-3.5" />
              <span>Copy</span>
            </>
          )}
        </button>
      </div>
      <pre className="p-3 text-xs md:text-sm font-mono overflow-x-auto text-ink leading-relaxed">
        <code>{code}</code>
      </pre>
    </div>
  );
}

function renderInline(text: string): React.ReactNode[] {
  const nodes: React.ReactNode[] = [];
  let pos = 0;
  let keyIndex = 0;

  while (pos < text.length) {
    // Inline code `...`
    if (text[pos] === "`") {
      const endTick = text.indexOf("`", pos + 1);
      if (endTick !== -1) {
        const codeText = text.slice(pos + 1, endTick);
        nodes.push(
          <code
            key={`code-${keyIndex++}`}
            className="px-1.5 py-0.5 rounded bg-panel2 text-ember text-xs font-mono border border-line"
          >
            {codeText}
          </code>
        );
        pos = endTick + 1;
        continue;
      }
    }

    // Bold **...**
    if (text.startsWith("**", pos)) {
      const endBold = text.indexOf("**", pos + 2);
      if (endBold !== -1) {
        const boldText = text.slice(pos + 2, endBold);
        nodes.push(
          <strong key={`bold-${keyIndex++}`} className="font-semibold text-ink">
            {renderInline(boldText)}
          </strong>
        );
        pos = endBold + 2;
        continue;
      }
    }

    // Italic *...*
    if (text[pos] === "*" && text[pos + 1] !== "*" && text[pos + 1] !== " ") {
      const endItalic = text.indexOf("*", pos + 1);
      if (endItalic !== -1 && endItalic > pos + 1) {
        const italicText = text.slice(pos + 1, endItalic);
        nodes.push(
          <em key={`italic-${keyIndex++}`} className="italic">
            {renderInline(italicText)}
          </em>
        );
        pos = endItalic + 1;
        continue;
      }
    }

    // Link [text](url)
    if (text[pos] === "[") {
      const closeBracket = text.indexOf("]", pos + 1);
      if (closeBracket !== -1 && text[closeBracket + 1] === "(") {
        const closeParen = text.indexOf(")", closeBracket + 2);
        if (closeParen !== -1) {
          const linkText = text.slice(pos + 1, closeBracket);
          const url = text.slice(closeBracket + 2, closeParen).trim();
          const isSafe = /^https?:\/\//i.test(url) || /^mailto:/i.test(url);
          if (isSafe) {
            nodes.push(
              <a
                key={`link-${keyIndex++}`}
                href={url}
                target="_blank"
                rel="noopener noreferrer"
                className="text-ember hover:underline"
              >
                {linkText}
              </a>
            );
          } else {
            nodes.push(<span key={`unsafe-${keyIndex++}`}>{linkText}</span>);
          }
          pos = closeParen + 1;
          continue;
        }
      }
    }

    // Accumulate plain text
    let nextSpecial = pos + 1;
    while (nextSpecial < text.length) {
      const ch = text[nextSpecial];
      if (ch === "`" || ch === "*" || ch === "[") break;
      nextSpecial++;
    }
    nodes.push(text.slice(pos, nextSpecial));
    pos = nextSpecial;
  }

  return nodes;
}

export function MarkdownRenderer({ content, isStreaming }: MarkdownRendererProps) {
  if (isStreaming && (!content || !content.trim())) {
    return (
      <div className="flex items-center gap-1.5 py-1 text-ink3" aria-label="Thinking">
        <style>{`
          @keyframes dot-cycle {
            0%, 80%, 100% { opacity: 0.2; }
            40% { opacity: 1; }
          }
          .dot-cycle-1 { animation: dot-cycle 1.4s infinite ease-in-out; }
          .dot-cycle-2 { animation: dot-cycle 1.4s infinite ease-in-out 0.2s; }
          .dot-cycle-3 { animation: dot-cycle 1.4s infinite ease-in-out 0.4s; }
        `}</style>
        <span className="w-1 h-1 rounded-full bg-ink3 dot-cycle-1" />
        <span className="w-1 h-1 rounded-full bg-ink3 dot-cycle-2" />
        <span className="w-1 h-1 rounded-full bg-ink3 dot-cycle-3" />
      </div>
    );
  }

  if (!content) return null;

  const lines = content.split(/\r?\n/);
  const elements: React.ReactNode[] = [];
  let i = 0;
  let elemKey = 0;

  while (i < lines.length) {
    const line = lines[i];

    // Fenced code block
    const fenceMatch = line.match(/^```(\w*)/);
    if (fenceMatch) {
      const lang = fenceMatch[1] || "";
      i++;
      const codeLines: string[] = [];
      while (i < lines.length) {
        if (/^```\s*$/.test(lines[i])) {
          i++;
          break;
        }
        codeLines.push(lines[i]);
        i++;
      }
      elements.push(
        <CodeBlock
          key={`block-${elemKey++}`}
          language={lang}
          code={codeLines.join("\n")}
        />
      );
      continue;
    }

    // Headings (#, ##, ###)
    const headingMatch = line.match(/^(#{1,3})\s+(.*)$/);
    if (headingMatch) {
      const level = headingMatch[1].length;
      const headingText = headingMatch[2];
      if (level === 1) {
        elements.push(
          <h2 key={`h1-${elemKey++}`} className="text-lg font-bold text-ink mt-3 mb-1">
            {renderInline(headingText)}
          </h2>
        );
      } else if (level === 2) {
        elements.push(
          <h3 key={`h2-${elemKey++}`} className="text-base font-semibold text-ink mt-2 mb-1">
            {renderInline(headingText)}
          </h3>
        );
      } else {
        elements.push(
          <h4 key={`h3-${elemKey++}`} className="text-sm font-semibold text-ink mt-2 mb-1">
            {renderInline(headingText)}
          </h4>
        );
      }
      i++;
      continue;
    }

    // Horizontal rule
    if (/^(\*{3,}|-{3,}|_{3,})\s*$/.test(line)) {
      elements.push(<hr key={`hr-${elemKey++}`} className="my-3 border-line" />);
      i++;
      continue;
    }

    // Unordered list
    if (/^[-*]\s+(.*)$/.test(line)) {
      const listItems: string[] = [];
      while (i < lines.length) {
        const itemMatch = lines[i].match(/^[-*]\s+(.*)$/);
        if (itemMatch) {
          listItems.push(itemMatch[1]);
          i++;
        } else if (lines[i].trim() === "") {
          break;
        } else {
          if (listItems.length > 0 && !/^(\d+\.|#{1,3}|```|[-*]\s+)/.test(lines[i])) {
            listItems[listItems.length - 1] += " " + lines[i].trim();
            i++;
          } else {
            break;
          }
        }
      }
      elements.push(
        <ul key={`ul-${elemKey++}`} className="list-disc list-inside space-y-1 my-2 text-ink">
          {listItems.map((item, idx) => (
            <li key={idx} className="leading-relaxed">
              {renderInline(item)}
            </li>
          ))}
        </ul>
      );
      continue;
    }

    // Ordered list
    if (/^\d+\.\s+(.*)$/.test(line)) {
      const listItems: string[] = [];
      while (i < lines.length) {
        const itemMatch = lines[i].match(/^\d+\.\s+(.*)$/);
        if (itemMatch) {
          listItems.push(itemMatch[1]);
          i++;
        } else if (lines[i].trim() === "") {
          break;
        } else {
          if (listItems.length > 0 && !/^(\d+\.|#{1,3}|```|[-*]\s+)/.test(lines[i])) {
            listItems[listItems.length - 1] += " " + lines[i].trim();
            i++;
          } else {
            break;
          }
        }
      }
      elements.push(
        <ol key={`ol-${elemKey++}`} className="list-decimal list-inside space-y-1 my-2 text-ink">
          {listItems.map((item, idx) => (
            <li key={idx} className="leading-relaxed">
              {renderInline(item)}
            </li>
          ))}
        </ol>
      );
      continue;
    }

    // Blank line
    if (line.trim() === "") {
      i++;
      continue;
    }

    // Paragraph
    const paraLines: string[] = [];
    while (i < lines.length) {
      const cur = lines[i];
      if (cur.trim() === "") break;
      if (/^```/.test(cur)) break;
      if (/^(#{1,3})\s+/.test(cur)) break;
      if (/^(\*{3,}|-{3,}|_{3,})\s*$/.test(cur)) break;
      if (/^[-*]\s+/.test(cur)) break;
      if (/^\d+\.\s+/.test(cur)) break;
      paraLines.push(cur);
      i++;
    }
    if (paraLines.length > 0) {
      elements.push(
        <p key={`p-${elemKey++}`} className="my-1.5 leading-relaxed text-ink">
          {paraLines.map((pLine, lIdx) => (
            <React.Fragment key={lIdx}>
              {renderInline(pLine)}
              {lIdx < paraLines.length - 1 && <br />}
            </React.Fragment>
          ))}
        </p>
      );
    }
  }

  if (isStreaming && elements.length > 0) {
    const caret = (
      <span
        key="streaming-caret"
        className="inline-block w-0.5 h-[1.1em] ml-0.5 align-middle bg-ember animate-pulse"
        aria-hidden="true"
      />
    );
    const lastIdx = elements.length - 1;
    const lastElem = elements[lastIdx];
    if (React.isValidElement(lastElem)) {
      const props = lastElem.props as any;
      if (lastElem.type === "ul" || lastElem.type === "ol") {
        if (Array.isArray(props.children) && props.children.length > 0) {
          const listItems = [...props.children];
          const lastLi = listItems[listItems.length - 1];
          if (React.isValidElement(lastLi)) {
            const liProps = lastLi.props as any;
            const liChildren = Array.isArray(liProps.children)
              ? [...liProps.children, caret]
              : [liProps.children, caret];
            listItems[listItems.length - 1] = React.cloneElement(lastLi, {}, liChildren);
            elements[lastIdx] = React.cloneElement(lastElem, {}, listItems);
          } else {
            elements.push(caret);
          }
        } else {
          elements.push(caret);
        }
      } else if (props && props.children !== undefined) {
        const children = Array.isArray(props.children)
          ? [...props.children, caret]
          : [props.children, caret];
        elements[lastIdx] = React.cloneElement(lastElem, {}, children);
      } else {
        elements.push(caret);
      }
    } else {
      elements.push(caret);
    }
  }

  return <div className="space-y-1">{elements}</div>;
}
