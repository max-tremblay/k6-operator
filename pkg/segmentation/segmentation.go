package segmentation

import (
	"errors"
	"fmt"
)

const (
	beginning = "0"
	end       = "1"
)

// NewCommandSequence builds command fragments for starting k6 with execution segments.
func NewCommandSegment(index int, total int) (string, error) {

	if index > total {
		return "", errors.New("node index exceeds configured parallelism")
	}

	getSegmentPart := func(index int, total int) string {
		if index == 0 {
			return "0"
		}
		if index == total {
			return "1"
		}
		return fmt.Sprintf("%d/%d", index, total)
	}

	segment := fmt.Sprintf("%s:%s", getSegmentPart(index-1, total), getSegmentPart(index, total))

	return fmt.Sprintf("--execution-segment=%s", segment), nil
}
