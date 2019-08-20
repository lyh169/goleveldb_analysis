// Copyright (c) 2012, Suryandaru Triandana <syndtr@gmail.com>
// All rights reserved.
//
// Use of this source code is governed by a BSD-style license that can be
// found in the LICENSE file.

package filter

import (
	"github.com/syndtr/goleveldb/leveldb/util"
)

func bloomHash(key []byte) uint32 {
	return util.Hash(key, 0xbc9f1d34)
}

type bloomFilter int

// Name: The bloom filter serializes its parameters and is backward compatible
// with respect to them. Therefor, its parameters are not added to its
// name.
func (bloomFilter) Name() string {
	return "leveldb.BuiltinBloomFilter"
}

func (f bloomFilter) Contains(filter, key []byte) bool {
	nBytes := len(filter) - 1  // filter的最好一位
	if nBytes < 1 {
		return false
	}
	nBits := uint32(nBytes * 8)

	// Use the encoded k so that we can read filters generated by
	// bloom filters created using different parameters.
	k := filter[nBytes]  // 最后一位写入的为k值，在Generate中实现的

	// 对于大于30个哈希函数的情况，这里直接返回存在
	if k > 30 {  // 30是在NewGenerator中指定的
		// Reserved for potentially new encodings for short bloom filters.
		// Consider it a match.
		return true
	}

	// Leveldb
	kh := bloomHash(key)
	delta := (kh >> 17) | (kh << 15) // Rotate right 17 bits  旋转右边17个bit
	for j := uint8(0); j < k; j++ {
		bitpos := kh % nBits  // 计算出实在哪个bit
		if (uint32(filter[bitpos/8]) & (1 << (bitpos % 8))) == 0 {
			return false  // 只要有一个桶不满足该数据，则该数据不在bloomFilter里面。
		}
		kh += delta  // 怎么写进去数据则怎么查询出来的
	}
	return true
}

func (f bloomFilter) NewGenerator() FilterGenerator {
	// Round down to reduce probing cost a little bit.
	k := uint8(f * 69 / 100) // 0.69 =~ ln(2)  // ln2(f)
	// 必须在[1, 30]之间
	if k < 1 {
		k = 1
	} else if k > 30 {
		k = 30
	}
	return &bloomFilterGenerator{
		n: int(f),
		k: k,
	}
}

type bloomFilterGenerator struct {
	n int  // m/n,一个key平均占多少个字节
	k uint8

	keyHashes []uint32
}

func (g *bloomFilterGenerator) Add(key []byte) {
	// Use double-hashing to generate a sequence of hash values.
	// See analysis in [Kirsch,Mitzenmacher 2006].
	g.keyHashes = append(g.keyHashes, bloomHash(key))
}

func (g *bloomFilterGenerator) Generate(b Buffer) {
	// Compute bloom filter size (in both bits and bytes)
	// 求出m
	nBits := uint32(len(g.keyHashes) * g.n)  // g.n为bloom filter的m/n,g.keyHashes为bloom filter中n的个数
	// For small n, we can see a very high false positive rate.  Fix it
	// by enforcing a minimum bloom filter length.

	// 对于n值很小的情况下，为了不让m受影响，所以m的最小值为64
	if nBits < 64 {
		nBits = 64   // 最少为64个bit
	}

	// 以Bytes数组代替Bit
	nBytes := (nBits + 7) / 8  // 计算实际占用多个byte，不足一个byte当一个byte
	nBits = nBytes * 8

	// 分配多一位Byte数组，最后一位把k值放进去
	dest := b.Alloc(int(nBytes) + 1)
	dest[nBytes] = g.k  // 最后一个byte值为g.k
	for _, kh := range g.keyHashes {
		delta := (kh >> 17) | (kh << 15) // Rotate right 17 bits
		// 单个kh被哈希了g.k次
		for j := uint8(0); j < g.k; j++ {
			bitpos := kh % nBits
			dest[bitpos/8] |= (1 << (bitpos % 8))  // 对应的bit打上1
			kh += delta
		}
	}

	// 将keyHashes的结果写入到Buffer里面然后置空
	g.keyHashes = g.keyHashes[:0]
}

// NewBloomFilter creates a new initialized bloom filter for given
// bitsPerKey.
//
// Since bitsPerKey is persisted individually for each bloom filter
// serialization, bloom filters are backwards compatible with respect to
// changing bitsPerKey. This means that no big performance penalty will
// be experienced when changing the parameter. See documentation for
// opt.Options.Filter for more information.

// bloomFilter中,m/n的值为bitsPerKey
func NewBloomFilter(bitsPerKey int) Filter {
	return bloomFilter(bitsPerKey)
}
