package connect

// Tracks encoded Transfer bytes without receiver delivery evidence. Reliable
// routes retain historical unlimited admission. A negotiated unreliable route
// starts below the low-bar queue and grows only from receiver delivery
// evidence. Transfer retains a hard admission cap and remains the sole payload
// retransmitter.
type sendFlightController struct {
	initialByteCount     ByteCount
	minimumByteCount     ByteCount
	maximumByteCount     ByteCount
	increaseByteCount    ByteCount
	initialMessageCount  int
	minimumMessageCount  int
	maximumMessageCount  int
	increaseMessageCount int
	slowStartDivisor     ByteCount

	activeMinimumByteCount    ByteCount
	activeMaximumByteCount    ByteCount
	policyByteLimit           ByteCount
	activeMinimumMessageCount int
	activeMaximumMessageCount int
	policyMessageLimit        int
	flowReserveEnabled        bool

	generation                 uint64
	limited                    bool
	slowStart                  bool
	byteCount                  ByteCount
	byteLimit                  ByteCount
	messageCount               int
	messageLimit               int
	messageCountByKey          map[sendSchedulingKey]int
	flowReserveInUse           bool
	additiveIncreaseRemainder  ByteCount
	slowStartIncreaseRemainder ByteCount
	additiveMessageRemainder   int
	slowStartMessageRemainder  int
}

// Normalizes optional settings once so the packet path has no configuration
// branches beyond the carrier-policy check.
func newSendFlightController(settings *SendBufferSettings) *sendFlightController {
	if settings == nil {
		return &sendFlightController{
			messageCountByKey: map[sendSchedulingKey]int{},
		}
	}
	initialByteCount := settings.UnreliableInitialFlightByteCount
	if initialByteCount <= 0 {
		return &sendFlightController{
			messageCountByKey: map[sendSchedulingKey]int{},
		}
	}
	minimumByteCount := settings.UnreliableMinimumFlightByteCount
	if minimumByteCount <= 0 {
		minimumByteCount = initialByteCount
	}
	maximumByteCount := settings.UnreliableMaximumFlightByteCount
	if maximumByteCount < minimumByteCount {
		maximumByteCount = minimumByteCount
	}
	initialByteCount = min(max(initialByteCount, minimumByteCount), maximumByteCount)
	increaseByteCount := settings.UnreliableFlightIncreaseByteCount
	if increaseByteCount <= 0 {
		increaseByteCount = 1
	}
	slowStartDivisor := ByteCount(settings.UnreliableSlowStartGrowthDivisor)
	if slowStartDivisor <= 0 {
		slowStartDivisor = 1
	}
	initialMessageCount := settings.UnreliableInitialFlightMessageCount
	minimumMessageCount := settings.UnreliableMinimumFlightMessageCount
	maximumMessageCount := settings.UnreliableMaximumFlightMessageCount
	increaseMessageCount := settings.UnreliableFlightIncreaseMessageCount
	if 0 < initialMessageCount {
		if minimumMessageCount <= 0 {
			minimumMessageCount = initialMessageCount
		}
		if maximumMessageCount < minimumMessageCount {
			maximumMessageCount = minimumMessageCount
		}
		initialMessageCount = min(max(initialMessageCount, minimumMessageCount), maximumMessageCount)
		if increaseMessageCount <= 0 {
			increaseMessageCount = 1
		}
	}
	return &sendFlightController{
		initialByteCount:     initialByteCount,
		minimumByteCount:     minimumByteCount,
		maximumByteCount:     maximumByteCount,
		increaseByteCount:    increaseByteCount,
		initialMessageCount:  initialMessageCount,
		minimumMessageCount:  minimumMessageCount,
		maximumMessageCount:  maximumMessageCount,
		increaseMessageCount: increaseMessageCount,
		slowStartDivisor:     slowStartDivisor,
		messageCountByKey:    map[sendSchedulingKey]int{},
	}
}

func (self *sendFlightController) applyPolicy(policy transferFlightPolicySnapshot) bool {
	limited := policy.limited && 0 < self.initialByteCount
	if self.generation == policy.generation && self.limited == limited &&
		self.policyByteLimit == policy.byteLimit &&
		self.policyMessageLimit == policy.messageLimit &&
		self.flowReserveEnabled == policy.flowReserve {
		return false
	}
	self.generation = policy.generation
	self.limited = limited
	self.policyByteLimit = policy.byteLimit
	self.policyMessageLimit = policy.messageLimit
	self.flowReserveEnabled = policy.flowReserve
	self.additiveIncreaseRemainder = 0
	self.slowStartIncreaseRemainder = 0
	self.additiveMessageRemainder = 0
	self.slowStartMessageRemainder = 0
	if limited {
		self.activeMaximumByteCount = self.maximumByteCount
		if 0 < policy.byteLimit {
			self.activeMaximumByteCount = min(
				self.activeMaximumByteCount,
				policy.byteLimit,
			)
		}
		self.activeMinimumByteCount = min(
			self.minimumByteCount,
			self.activeMaximumByteCount,
		)
		self.byteLimit = min(
			max(self.initialByteCount, self.activeMinimumByteCount),
			self.activeMaximumByteCount,
		)
		self.activeMinimumMessageCount = self.minimumMessageCount
		self.activeMaximumMessageCount = self.maximumMessageCount
		if 0 < policy.messageLimit && 0 < self.initialMessageCount {
			self.activeMaximumMessageCount = min(
				self.activeMaximumMessageCount,
				policy.messageLimit,
			)
			self.activeMinimumMessageCount = min(
				self.activeMinimumMessageCount,
				self.activeMaximumMessageCount,
			)
		}
		self.messageLimit = min(
			max(self.initialMessageCount, self.activeMinimumMessageCount),
			self.activeMaximumMessageCount,
		)
		self.slowStart = true
	} else {
		self.byteLimit = 0
		self.activeMinimumByteCount = 0
		self.activeMaximumByteCount = 0
		self.messageLimit = 0
		self.activeMinimumMessageCount = 0
		self.activeMaximumMessageCount = 0
		self.slowStart = false
	}
	return true
}

func (self *sendFlightController) canSend() bool {
	return self.canSendForKey(sendSchedulingKey{})
}

func (self *sendFlightController) canSendForKey(key sendSchedulingKey) bool {
	if !self.limited || (self.byteCount == 0 && self.messageCount == 0) {
		return true
	}
	if self.byteCount < self.byteLimit &&
		(self.messageLimit <= 0 || self.messageCount < self.messageLimit) {
		return true
	}
	return self.flowReserveEnabled && key.valid && !self.flowReserveInUse &&
		self.messageCountByKey[key] == 0
}

func (self *sendFlightController) send(byteCount ByteCount) {
	self.sendForKey(byteCount, sendSchedulingKey{})
}

func (self *sendFlightController) sendForKey(
	byteCount ByteCount,
	key sendSchedulingKey,
) bool {
	reserved := self.limited && self.flowReserveEnabled && key.valid &&
		!self.flowReserveInUse &&
		self.messageCountByKey[key] == 0 &&
		(self.byteLimit <= self.byteCount ||
			0 < self.messageLimit && self.messageLimit <= self.messageCount)
	if reserved {
		self.flowReserveInUse = true
	}
	if 0 < byteCount {
		self.byteCount += byteCount
		self.messageCount += 1
		if key.valid {
			self.messageCountByKey[key] += 1
		}
	}
	return reserved
}

func (self *sendFlightController) acknowledge(byteCount ByteCount) {
	self.acknowledgeForKey(byteCount, sendSchedulingKey{}, false)
}

func (self *sendFlightController) acknowledgeForKey(
	byteCount ByteCount,
	key sendSchedulingKey,
	reserved bool,
) {
	// The reserve is a per-scheduling-key fairness slot, not a byte
	// allocation: sendForKey grants it outside the byte guard, so release it
	// outside the byte guard too. Releasing after the guard would strand the
	// slot forever on a zero-byte acknowledgement.
	if reserved {
		self.flowReserveInUse = false
	}
	if byteCount <= 0 {
		return
	}
	acknowledgedByteCount := min(byteCount, self.byteCount)
	self.byteCount -= acknowledgedByteCount
	acknowledgedMessageCount := 0
	if 0 < self.messageCount {
		self.messageCount -= 1
		acknowledgedMessageCount = 1
		if key.valid && 0 < self.messageCountByKey[key] {
			self.messageCountByKey[key] -= 1
			if self.messageCountByKey[key] == 0 {
				delete(self.messageCountByKey, key)
			}
		}
	}
	if !self.limited {
		return
	}
	if self.slowStart {
		if 0 < acknowledgedByteCount && self.byteLimit < self.activeMaximumByteCount {
			numerator := self.slowStartIncreaseRemainder + acknowledgedByteCount
			increaseByteCount := numerator / self.slowStartDivisor
			self.slowStartIncreaseRemainder = numerator % self.slowStartDivisor
			self.byteLimit = min(
				self.activeMaximumByteCount,
				self.byteLimit+increaseByteCount,
			)
		}
		if 0 < acknowledgedMessageCount &&
			self.messageLimit < self.activeMaximumMessageCount {
			numerator := self.slowStartMessageRemainder + acknowledgedMessageCount
			divisor := int(self.slowStartDivisor)
			increaseMessageCount := numerator / divisor
			self.slowStartMessageRemainder = numerator % divisor
			self.messageLimit = min(
				self.activeMaximumMessageCount,
				self.messageLimit+increaseMessageCount,
			)
		}
		return
	}

	if 0 < acknowledgedByteCount && self.byteLimit < self.activeMaximumByteCount {
		numerator := self.additiveIncreaseRemainder +
			self.increaseByteCount*acknowledgedByteCount
		increaseByteCount := numerator / self.byteLimit
		self.additiveIncreaseRemainder = numerator % self.byteLimit
		self.byteLimit = min(
			self.activeMaximumByteCount,
			self.byteLimit+increaseByteCount,
		)
	}
	if 0 < acknowledgedMessageCount && 0 < self.messageLimit &&
		self.messageLimit < self.activeMaximumMessageCount {
		numerator := self.additiveMessageRemainder +
			self.increaseMessageCount*acknowledgedMessageCount
		increaseMessageCount := numerator / self.messageLimit
		self.additiveMessageRemainder = numerator % self.messageLimit
		self.messageLimit = min(
			self.activeMaximumMessageCount,
			self.messageLimit+increaseMessageCount,
		)
	}
}

func (self *sendFlightController) reduceForLoss() bool {
	if !self.limited {
		return false
	}
	self.slowStart = false
	self.additiveIncreaseRemainder = 0
	self.slowStartIncreaseRemainder = 0
	self.additiveMessageRemainder = 0
	self.slowStartMessageRemainder = 0
	reducedByteLimit := max(self.activeMinimumByteCount, self.byteLimit/2)
	reduced := self.byteLimit > reducedByteLimit
	self.byteLimit = reducedByteLimit
	if 0 < self.messageLimit {
		reducedMessageLimit := max(
			self.activeMinimumMessageCount,
			self.messageLimit/2,
		)
		reduced = reduced || self.messageLimit > reducedMessageLimit
		self.messageLimit = reducedMessageLimit
	}
	return reduced
}

func (self *sendFlightController) atFloor() bool {
	if !self.limited || self.slowStart {
		return false
	}
	if self.byteLimit <= self.activeMinimumByteCount {
		return true
	}
	return 0 < self.messageLimit && self.messageLimit <= self.activeMinimumMessageCount
}

type sendSchedulingKey struct {
	valid bool
}

type sequenceTag struct {
	sendTime uint64
	set      bool
}
