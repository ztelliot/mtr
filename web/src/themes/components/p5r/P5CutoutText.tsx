export function P5CutoutText({ text, variant = "word" }: { text: string; variant?: "word" | "label" }) {
  const letters = Array.from(text);
  return (
    <span className={`p5-cutout p5-cutout-${variant}`} data-compact={(variant === "label" && letters.length > 7) || undefined}>
      <span aria-hidden="true">
        {letters.map((letter, i) => <span className="p5-cut-letter" key={i}>{letter}</span>)}
      </span>
      <span className="p5-screen-reader-text">{text}</span>
    </span>
  );
}
