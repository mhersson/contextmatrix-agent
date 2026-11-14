package runkind

// GuidanceMarkerPrefix starts every guidance block marker. A guidance section
// may not contain it, so a section can never forge or close a block.
const GuidanceMarkerPrefix = "<!-- sentinel:"
