// Procedural product illustrations: the demo brands are invented, so there are no photos to use.
const hues: Record<string, number> = {
  p_halvo_q7: 258,
  p_brisane_anc2: 205,
  p_orrin_air: 160,
  p_tellum_pro: 20,
  p_wendmere_c22: 280,
  p_pallis_s: 35,
  p_quorra_glide: 210,
  p_ostrel_l48: 150,
  p_benfold_rise: 15,
  p_kavari_duo: 330,
};

export function ProductArt({ id, category, className = "" }: { id: string; category: string; className?: string }) {
  const h = hues[id] ?? 250;
  const main = `hsl(${h} 55% 46%)`;
  const dark = `hsl(${h} 45% 28%)`;
  const soft = `hsl(${h} 70% 94%)`;
  return (
    <svg viewBox="0 0 120 90" className={className} role="img" aria-label={`${category} illustration`}>
      <rect width="120" height="90" rx="14" fill={soft} />
      {category === "headphones" && (
        <g>
          <path d="M34 54 C34 26, 86 26, 86 54" fill="none" stroke={dark} strokeWidth="6" strokeLinecap="round" />
          <rect x="24" y="48" width="18" height="26" rx="8" fill={main} />
          <rect x="78" y="48" width="18" height="26" rx="8" fill={main} />
          <rect x="29" y="53" width="8" height="16" rx="4" fill={soft} opacity="0.35" />
        </g>
      )}
      {category === "luggage" && (
        <g>
          <rect x="52" y="14" width="16" height="10" rx="3" fill="none" stroke={dark} strokeWidth="4" />
          <rect x="38" y="22" width="44" height="52" rx="8" fill={main} />
          <line x1="52" y1="30" x2="52" y2="66" stroke={soft} strokeOpacity="0.4" strokeWidth="3" />
          <line x1="68" y1="30" x2="68" y2="66" stroke={soft} strokeOpacity="0.4" strokeWidth="3" />
          <circle cx="46" cy="78" r="4" fill={dark} />
          <circle cx="74" cy="78" r="4" fill={dark} />
        </g>
      )}
      {category === "desks" && (
        <g>
          <rect x="18" y="30" width="84" height="8" rx="3" fill={main} />
          <rect x="30" y="38" width="7" height="38" rx="2" fill={dark} />
          <rect x="83" y="38" width="7" height="38" rx="2" fill={dark} />
          <rect x="24" y="74" width="20" height="4" rx="2" fill={dark} />
          <rect x="76" y="74" width="20" height="4" rx="2" fill={dark} />
          <rect x="48" y="16" width="26" height="14" rx="2" fill={dark} opacity="0.85" />
        </g>
      )}
    </svg>
  );
}
