export const tierColor = (t: string): string =>
  ({ free: '#10b981', standard: '#06b6d4', premium: '#7c3aed' } as Record<string, string>)[t] || '#64748b'