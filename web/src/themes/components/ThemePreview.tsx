import { themes, type UITheme } from "../index";

export function ThemePreview({ theme }: { theme: UITheme }) {
  const [background, surface, accent] = themes[theme].swatches;
  return <span className="theme-preview" data-theme={theme} data-navigation={themes[theme].navigation} aria-hidden="true">
    <svg viewBox="0 0 64 44" width="64" height="44">
      <rect width="64" height="44" rx={theme === "material" ? 6 : theme === "classic" || theme === "fluent" ? 3 : 0} fill={theme === "p5r" ? accent : background} />
      {theme === "pixel" && <>
        {Array.from({ length: 40 }, (_, i) => <rect key={i} x={3 + i % 10 * 6} y={3 + Math.floor(i / 10) * 11} width="1" height="1" fill="#a6c5bf" />)}
        <path d="M4 5h12v3H4zm19 0h7v3h-7zm11 0h7v3h-7zm11 0h7v3h-7z" fill={accent} />
        <rect x="4" y="14" width="56" height="12" fill="#fff" stroke="#59716d" />
        <path d="M7 18h28v4H7zm34 0h14v4H41z" fill={surface} stroke={accent} strokeWidth=".6" />
        <rect x="4" y="30" width="56" height="12" fill="#fff" stroke="#59716d" strokeWidth=".7" />
        {Array.from({ length: 18 }, (_, i) => (
          <rect key={i} x={8 + i % 9 * 6} y={33 + Math.floor(i / 9) * 6} width="1" height="1" fill={accent} opacity=".6" />
        ))}
      </>}
      {theme === "classic" && <>
        <path d="M4 5h13v3H4zm22 0h7v2h-7zm11 0h7v2h-7zm11 0h7v2h-7z" fill={accent} />
        <rect x="4" y="15" width="56" height="12" rx="2" fill="#fff" stroke="#d0d6de" />
        <rect x="7" y="18" width="30" height="5" rx="1" fill="#fff" stroke="#cad1da" />
        <rect x="43" y="18" width="13" height="5" rx="1" fill={accent} />
        <rect x="4" y="32" width="56" height="10" rx="2" fill={surface} />
      </>}
      {theme === "metro" && <>
        <path d="M4 3h14v2H4z" fill={accent} />
        {Array.from({ length: 6 }, (_, i) => <rect key={i} x={4 + i * 9.5} y="8" width="8" height="8" fill={i === 0 || i === 5 ? accent : "#c8e0f0"} />)}
        <rect x="4" y="21" width="56" height="11" fill={accent} />
        <path d="M7 25h32v4H7zm39 0h10v4H46z" fill="white" />
        <rect x="4" y="36" width="56" height="6" fill="#fff" stroke="#9bb6cc" strokeWidth=".7" />
      </>}
      {theme === "fluent" && <>
        <rect x="2" y="2" width="13" height="40" rx="2" fill="#e7e7e7" />
        <path d="M5 5h7v2H5z" fill="#383838" />
        <rect x="4" y="11" width="9" height="5" rx="1" fill="#d0e8fa" />
        <path d="M4 12v3" stroke={accent} strokeWidth="1.5" />
        <path d="M6 20h5m-5 6h5m-5 6h5" stroke="#7b7b7b" strokeWidth="1.4" />
        <path d="M20 5h17v2H20z" fill="#444" />
        <rect x="20" y="12" width="40" height="13" rx="2" fill={surface} stroke="#dedede" />
        <rect x="46" y="17" width="10" height="5" rx="1" fill={accent} />
        <rect x="20" y="30" width="40" height="12" rx="2" fill="white" stroke="#dedede" />
      </>}
      {theme === "material" && <>
        <rect x="3" y="7" width="9" height="5" rx="3" fill={surface} />
        <path d="M7.5 8v3m-1.5-1.5h3" stroke={accent} />
        {[19, 26, 33].map(y => <circle key={y} cx="7.5" cy={y} r="1.4" fill="#81768b" />)}
        <rect x="16" y="3" width="46" height="39" rx="7" fill="#f0e8f6" />
        <path d="M21 8h14v2H21z" fill={accent} />
        <rect x="20" y="15" width="38" height="11" rx="4" fill={surface} />
        <rect x="24" y="19" width="18" height="4" rx="1" fill="white" />
        <rect x="46" y="19" width="8" height="4" rx="3" fill={accent} />
        <rect x="20" y="30" width="38" height="9" rx="4" fill="white" />
      </>}
      {theme === "p5r" && <>
        <path d="M3 3l15-1 1 5L3 8z" fill="#08080a" />
        {[4, 22, 40].map(x => <g key={x}><path d={`M${x} 11l18-2-2 8-18 2z`} fill={surface} /><path d={`M${x + 2} 12l13-1-1 4-13 1z`} fill="#08080a" /></g>)}
        <rect x="3" y="22" width="58" height="13" fill="#08080a" stroke={surface} strokeWidth="1.5" />
        <circle cx="15" cy="28" r="9" fill="none" stroke={accent} strokeWidth="3" />
        <path d="M7 26h30v5H7z" fill="#08080a" stroke={surface} strokeWidth=".7" />
        <path d="M41 20l18-1 1 8-18 1z" fill="#08080a" stroke={surface} strokeWidth=".8" />
        <path d="M44 19l4-1 .5 1.5-4 1z" fill={accent} />
        <path d="M44 23h8m3-1v3" stroke={surface} strokeWidth="1.5" />
        <rect x="3" y="38" width="58" height="5" fill="#08080a" stroke={surface} strokeWidth="1.5" />
      </>}
    </svg>
  </span>;
}
