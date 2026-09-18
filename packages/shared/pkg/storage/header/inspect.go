package header

import (
	"errors"
	"fmt"
	"strings"

	"github.com/google/uuid"
)

// Format returns a string representation of the mapping as:
//
// startBlock-endBlock [offset, offset+length) := [buildStorageOffset, buildStorageOffset+length) ⊂ buildId, length in bytes
//
// It is used for debugging and visualization.
func (mapping BuildMap) Format(blockSize uint64) string {
	rangeMessage := fmt.Sprintf("%d-%d", mapping.Offset/blockSize, (mapping.Offset+mapping.Length)/blockSize)

	return fmt.Sprintf(
		"%-14s [%11d,%11d) := [%11d,%11d) ⊂ %s, %d B",
		rangeMessage,
		mapping.Offset, mapping.Offset+mapping.Length,
		mapping.BuildStorageOffset, mapping.BuildStorageOffset+mapping.Length, mapping.BuildId.String(), mapping.Length,
	)
}

const (
	SkippedBlockChar = '░'
	DirtyBlockChar1  = '▓'
	DirtyBlockChar2  = '█'
)

// maxVisualizeCells bounds the grid Visualize allocates. size and blockSize are
// parsed from the artifact under inspection, so the cell count is checked
// before the allocation instead of trusting the parsed values.
const maxVisualizeCells = 1 << 20

// Layers returns the set of buildIds that are present in the mappings.
func Layers(mappings []BuildMap) map[uuid.UUID]struct{} {
	layers := make(map[uuid.UUID]struct{}, len(mappings))

	for _, mapping := range mappings {
		layers[mapping.BuildId] = struct{}{}
	}

	return layers
}

// Visualize returns a string representation of the mappings as a grid of blocks.
// It is used for debugging and visualization.
//
// You can pass maps to visualize different groups of buildIds.
//
// size and blockSize come from the header being inspected, so the grid is
// bounded before allocation and every write is checked against it: a malformed
// or hostile artifact fails here instead of panicking or exhausting memory.
func Visualize(mappings []BuildMap, size, blockSize, cols uint64, bottomGroup, topGroup map[uuid.UUID]struct{}) (string, error) {
	if blockSize == 0 {
		return "", errors.New("visualize: block size must not be zero")
	}

	if cols == 0 {
		return "", errors.New("visualize: column count must not be zero")
	}

	if size%blockSize != 0 {
		return "", fmt.Errorf("visualize: size %d is not a multiple of the block size %d", size, blockSize)
	}

	cells := size / blockSize
	if cells > maxVisualizeCells {
		return "", fmt.Errorf("visualize: %d cells exceeds the %d-cell limit", cells, maxVisualizeCells)
	}

	output := make([]rune, cells)
	for outputIdx := range output {
		output[outputIdx] = SkippedBlockChar
	}

	for _, mapping := range mappings {
		for block := range mapping.Length / blockSize {
			idx := mapping.Offset/blockSize + block
			if idx >= uint64(len(output)) {
				return "", fmt.Errorf("visualize: mapping %s writes past the %d-cell grid", mapping.Format(blockSize), len(output))
			}

			if bottomGroup != nil {
				if _, ok := bottomGroup[mapping.BuildId]; ok {
					output[idx] = DirtyBlockChar1
				}
			}

			if topGroup != nil {
				if _, ok := topGroup[mapping.BuildId]; ok {
					output[idx] = DirtyBlockChar2
				}
			}
		}
	}

	lineOutput := make([]string, 0, cells/cols+1)

	for i := uint64(0); i < cells; i += cols {
		if i+cols <= uint64(len(output)) {
			lineOutput = append(lineOutput, string(output[i:i+cols]))
		} else {
			lineOutput = append(lineOutput, string(output[i:]))
		}
	}

	return strings.Join(lineOutput, "\n"), nil
}

// ValidateMappings validates the mappings.
// It is used to check if the mappings are valid.
//
// It checks if the mappings are contiguous and if the length of each mapping is a multiple of the block size.
// It also checks if the mappings cover the whole size.
func ValidateMappings(mappings []BuildMap, size, blockSize uint64) error {
	if len(mappings) == 0 {
		// An empty mapping set covers exactly nothing; anything larger is a
		// malformed header and must fail here rather than index mappings[-1].
		if size == 0 {
			return nil
		}

		return fmt.Errorf("mapping validation failed: no mappings cover size %d", size)
	}

	var currentOffset uint64

	for _, mapping := range mappings {
		if currentOffset != mapping.Offset {
			return fmt.Errorf("mapping validation failed: the following mapping\n- %s\ndoes not start at the correct offset: expected %d (block %d), got %d (block %d)", mapping.Format(blockSize), currentOffset, currentOffset/blockSize, mapping.Offset, mapping.Offset/blockSize)
		}

		if mapping.Length%blockSize != 0 {
			return fmt.Errorf("mapping validation failed: the following mapping\n- %s\nhas an invalid length: %d. It should be a multiple of block size: %d", mapping.Format(blockSize), mapping.Length, blockSize)
		}

		if currentOffset+mapping.Length > size {
			return fmt.Errorf("mapping validation failed: the following mapping\n- %s\ngoes beyond the size: %d (current offset) + %d (length) > %d (size)", mapping.Format(blockSize), currentOffset, mapping.Length, size)
		}

		currentOffset += mapping.Length
	}

	if currentOffset != size {
		return fmt.Errorf("mapping validation failed: the following mapping\n- %s\ndoes not cover the whole size: %d (current offset) != %d (size)", mappings[len(mappings)-1].Format(blockSize), currentOffset, size)
	}

	return nil
}

// Equal reports whether two mappings address the same device range, build and
// storage region. BuildStorageOffset is part of the identity: two mappings with
// equal ranges but different storage offsets read different bytes.
func (mapping BuildMap) Equal(other BuildMap) bool {
	return mapping.Offset == other.Offset &&
		mapping.Length == other.Length &&
		mapping.BuildId == other.BuildId &&
		mapping.BuildStorageOffset == other.BuildStorageOffset
}

func Equal(a, b []BuildMap) bool {
	if len(a) != len(b) {
		return false
	}

	for i := range a {
		if !a[i].Equal(b[i]) {
			return false
		}
	}

	return true
}
