// The herd mark. One copy: the desktop and mobile headers each carried the
// full SVG, identical but for its size.
export function Logo({ size }: { size: number }) {
  return (
    <svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 64 64" width={size} height={size} className="flex-none" aria-hidden>
      <line x1="32" y1="32" x2="49.85" y2="42.3" stroke="currentColor" strokeOpacity="0.38" strokeWidth="2.65"></line>
      <line x1="32" y1="32" x2="32" y2="52.61" stroke="#f2749b" strokeOpacity="0.7" strokeWidth="2.65"></line>
      <line x1="32" y1="32" x2="14.15" y2="42.3" stroke="currentColor" strokeOpacity="0.38" strokeWidth="2.65"></line>
      <line x1="32" y1="32" x2="14.15" y2="21.7" stroke="currentColor" strokeOpacity="0.38" strokeWidth="2.65"></line>
      <line x1="32" y1="32" x2="32" y2="11.39" stroke="currentColor" strokeOpacity="0.38" strokeWidth="2.65"></line>
      <line x1="32" y1="32" x2="49.85" y2="21.7" stroke="currentColor" strokeOpacity="0.38" strokeWidth="2.65"></line>
      <polygon points="49.85,51.14 42.2,46.72 42.2,37.89 49.85,33.47 57.5,37.89 57.5,46.72" fill="#7d6fae"></polygon>
      <polygon points="32,61.44 24.35,57.02 24.35,48.19 32,43.78 39.65,48.19 39.65,57.02" fill="#f2749b"></polygon>
      <polygon points="14.15,51.14 6.5,46.72 6.5,37.89 14.15,33.47 21.8,37.89 21.8,46.72" fill="#7d6fae"></polygon>
      <polygon points="14.15,30.53 6.5,26.11 6.5,17.28 14.15,12.86 21.8,17.28 21.8,26.11" fill="#7d6fae"></polygon>
      <polygon points="32,20.22 24.35,15.81 24.35,6.98 32,2.56 39.65,6.98 39.65,15.81" fill="#7d6fae"></polygon>
      <polygon points="49.85,30.53 42.2,26.11 42.2,17.28 49.85,12.86 57.5,17.28 57.5,26.11" fill="#7d6fae"></polygon>
      <polygon points="32,40.83 24.35,36.42 24.35,27.58 32,23.17 39.65,27.58 39.65,36.42" fill="currentColor"></polygon>
    </svg>
  );
}
