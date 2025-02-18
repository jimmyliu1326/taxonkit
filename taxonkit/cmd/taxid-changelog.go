// Copyright © 2016-2022 Wei Shen <shenwei356@gmail.com>
//
// Permission is hereby granted, free of charge, to any person obtaining a copy
// of this software and associated documentation files (the "Software"), to deal
// in the Software without restriction, including without limitation the rights
// to use, copy, modify, merge, publish, distribute, sublicense, and/or sell
// copies of the Software, and to permit persons to whom the Software is
// furnished to do so, subject to the following conditions:
//
// The above copyright notice and this permission notice shall be included in
// all copies or substantial portions of the Software.
//
// THE SOFTWARE IS PROVIDED "AS IS", WITHOUT WARRANTY OF ANY KIND, EXPRESS OR
// IMPLIED, INCLUDING BUT NOT LIMITED TO THE WARRANTIES OF MERCHANTABILITY,
// FITNESS FOR A PARTICULAR PURPOSE AND NONINFRINGEMENT. IN NO EVENT SHALL THE
// AUTHORS OR COPYRIGHT HOLDERS BE LIABLE FOR ANY CLAIM, DAMAGES OR OTHER
// LIABILITY, WHETHER IN AN ACTION OF CONTRACT, TORT OR OTHERWISE, ARISING FROM,
// OUT OF OR IN CONNECTION WITH THE SOFTWARE OR THE USE OR OTHER DEALINGS IN
// THE SOFTWARE.

package cmd

import (
	"bytes"
	"encoding/csv"
	"fmt"
	"io/ioutil"
	"path/filepath"
	"sort"
	"strings"
	"sync"

	"github.com/shenwei356/util/pathutil"
	"github.com/shenwei356/xopen"
	"github.com/spf13/cobra"
	"github.com/twotwotwo/sorts"
)

// taxidlogCmd represents the taxid-changelog command
var taxidlogCmd = &cobra.Command{
	Use:   "taxid-changelog",
	Short: "Create TaxId changelog from dump archives",
	Long: `Create TaxId changelog from dump archives

Steps:

    # dependencies:
    #   rush - https://github.com/shenwei356/rush/

    mkdir -p archive; cd archive;

    # --------- download ---------

    # option 1
    # for fast network connection
    wget ftp://ftp.ncbi.nlm.nih.gov/pub/taxonomy/taxdump_archive/taxdmp*.zip

    # option 2
    # for slow network connection
    url=https://ftp.ncbi.nlm.nih.gov/pub/taxonomy/taxdump_archive/
    wget $url -O - -o /dev/null \
        | grep taxdmp | perl -ne '/(taxdmp_.+?.zip)/; print "$1\n";' \
        | rush -j 2 -v url=$url 'axel -n 5 {url}/{}' \
            --immediate-output  -c -C download.rush

    # --------- unzip ---------

    ls taxdmp*.zip | rush -j 1 'unzip {} names.dmp nodes.dmp merged.dmp delnodes.dmp -d {@_(.+)\.}'

    # optionally compress .dmp files with pigz, for saving disk space
    fd .dmp$ | rush -j 4 'pigz {}'

    # --------- create log ---------

    cd ..
    taxonkit taxid-changelog -i archive -o taxid-changelog.csv.gz --verbose

Output format (CSV):

    # fields        comments
    taxid           # taxid
    version         # version / time of archive, e.g, 2019-07-01
    change          # change, values:
                    #   NEW             newly added
                    #   REUSE_DEL       deleted taxids being reused
                    #   REUSE_MER       merged taxids being reused
                    #   DELETE          deleted
                    #   MERGE           merged into another taxid
                    #   ABSORB          other taxids merged into this one
                    #   CHANGE_NAME     scientific name changed
                    #   CHANGE_RANK     rank changed
                    #   CHANGE_LIN_LIN  lineage taxids remain but lineage changed
                    #   CHANGE_LIN_TAX  lineage taxids changed
                    #   CHANGE_LIN_LEN  lineage length changed
    change-value    # variable values for changes: 
                    #   1) new taxid for MERGE
                    #   2) merged taxids for ABSORB
                    #   3) empty for others
    name            # scientific name
    rank            # rank
    lineage         # full lineage of the taxid
    lineage-taxids  # taxids of the lineage

    # you can use csvtk to investigate them. e.g.,
    csvtk grep -f taxid -p 1390515 taxid-changelog.csv.gz
`,
	Run: func(cmd *cobra.Command, args []string) {
		config := getConfigs(cmd)

		archivePath := getFlagString(cmd, "archive")
		if archivePath == "" {
			checkError(fmt.Errorf("flag -i/--archive needed"))
		}

		dirs := checkArchives(config, archivePath)
		createChangelog(config, archivePath, dirs)
	},
}

func init() {
	RootCmd.AddCommand(taxidlogCmd)

	taxidlogCmd.Flags().StringP("archive", "i", "", "directory containing uncompressed dumped archives")
}

// TaxidChangeCode represents code of taxid change type
type TaxidChangeCode uint32

const (
	// TaxidUnchanged means taxid not changed
	TaxidUnchanged TaxidChangeCode = iota
	// TaxidNew means a new taxid
	TaxidNew
	// TaxidReuseDeleted means deleted taxids are reused again
	TaxidReuseDeleted
	// TaxidReuseMerged means merged taxids are reused again
	TaxidReuseMerged
	// TaxidDelete means a taxid is deleted
	TaxidDelete
	// TaxidMerge means a taxid is merged into another taxid
	TaxidMerge
	// TaxidAbsorb means another taxid is merged into this one
	TaxidAbsorb
	// TaxidNameChanged means scientific name is changed
	TaxidNameChanged
	// TaxidRankChanged means the rank is changed
	TaxidRankChanged
	// TaxidLineageChangedLin means lineage taxids remain, but lineage changed
	TaxidLineageChangedLin
	// TaxidLineageChangedTax means lineage taxids changed
	TaxidLineageChangedTax
	// TaxidLineageChangedLen means number of lineage taxids changed
	TaxidLineageChangedLen // number of lineage taxids changed
)

func (c TaxidChangeCode) String() string {
	switch c {
	case TaxidNew:
		return "NEW"
	case TaxidReuseDeleted:
		return "REUSE_DEL"
	case TaxidReuseMerged:
		return "REUSE_MER"
	case TaxidDelete:
		return "DELETE"
	case TaxidMerge:
		return "MERGE"
	case TaxidAbsorb:
		return "ABSORB"
	case TaxidNameChanged:
		return "CHANGE_NAME"
	case TaxidRankChanged:
		return "CHANGE_RANK"
	case TaxidLineageChangedLin:
		return "CHANGE_LIN_LIN"
	case TaxidLineageChangedTax:
		return "CHANGE_LIN_TAX"
	case TaxidLineageChangedLen:
		return "CHANGE_LIN_LEN"
	case TaxidUnchanged:
		return "UNCHANGE"
	}
	return "UNDEFINED TaxidChangeCode"
}

func linegeChangeType(a, b []uint32, taxid2names map[int16]map[uint32]string, va, vb int16) TaxidChangeCode {
	if (a == nil) != (b == nil) {
		return TaxidLineageChangedLen
	}

	if len(a) != len(b) {
		return TaxidLineageChangedLen
	}

	for i, v := range a {
		if v != b[i] {
			return TaxidLineageChangedTax
		}
	}

	for i, v := range a {
		if taxid2names[va][v] != taxid2names[vb][b[i]] {
			return TaxidLineageChangedLin
		}
	}

	return TaxidUnchanged
}

// TaxidChange means a taxid change record
type TaxidChange struct {
	Version       int16
	TaxidVersion  int16
	Change        TaxidChangeCode
	LineageTaxids []uint32
	ChangeValue   []uint32
}

// TaxidChanges represents a list of TaxidChange
type TaxidChanges []TaxidChange

func (changes TaxidChanges) Len() int { return len(changes) }

func (changes TaxidChanges) Swap(i, j int) { changes[i], changes[j] = changes[j], changes[i] }

func (changes TaxidChanges) Less(i, j int) bool {
	if changes[i].Version < changes[j].Version {
		return true
	}
	if changes[i].Version > changes[j].Version {
		return false
	}
	if changes[i].Change < changes[j].Change {
		return true
	}
	return false
}

func (c TaxidChange) String() string {
	var buf bytes.Buffer
	var tmp []string
	var tid uint32
	var i int

	// version
	buf.WriteString(fmt.Sprintf("%d", c.Version))

	// taxid version
	buf.WriteString(fmt.Sprintf(",%d", c.TaxidVersion))

	// change
	buf.WriteString(fmt.Sprintf(",%s", c.Change))

	// change value
	buf.WriteString(",")
	switch c.Change {
	case TaxidMerge:
		buf.WriteString(fmt.Sprintf("%d", c.ChangeValue[0]))
	case TaxidAbsorb:
		tmp = make([]string, len(c.ChangeValue))
		for i, tid = range c.ChangeValue {
			tmp[i] = fmt.Sprintf("%d", tid)
		}
		buf.WriteString(strings.Join(tmp, ";"))
	}

	// lineage-taxid
	buf.WriteString(",")
	if c.LineageTaxids != nil {
		tmp = make([]string, len(c.LineageTaxids))
		for i, tid = range c.LineageTaxids {
			tmp[i] = fmt.Sprintf("%d", tid)
		}
		buf.WriteString(strings.Join(tmp, ";"))
	}

	return buf.String()
}

func createChangelog(config Config, path string, dirs []string) {
	outfh, err := xopen.Wopen(config.OutFile)
	checkError(err)
	defer outfh.Close()

	writer := csv.NewWriter(outfh)
	defer func() {
		writer.Flush()
		checkError(writer.Error())
	}()

	// taxid -> change-code -> []changes
	data := make(map[uint32][]TaxidChange, 1<<10)

	allMerges := make(map[uint32]uint32, 1<<10)

	// version -> taxid -> name
	taxid2names := make(map[int16]map[uint32]string, len(dirs))

	// version -> taxid -> rank
	taxid2ranks := make(map[int16]map[uint32]string, len(dirs))

	// versions
	sort.Strings(dirs)
	versions := dirs

	var ok bool
	var changes []TaxidChange
	var prevChange *TaxidChange
	var changeCode TaxidChangeCode
	var from, to, prevTo uint32
	var toRecord bool
	for version, dir := range dirs {
		if config.Verbose {
			log.Infof("parsing archive (%2d / %2d): %s", version+1, len(dirs), dir)
		}

		if config.Verbose {
			log.Infof("  loading data ...")
		}

		var taxid2lineageTaxids map[uint32][]uint32
		var taxid2rank map[uint32]string
		var taxid2name map[uint32]string
		var delTaxids []uint32
		var merges [][2]uint32

		var wg sync.WaitGroup
		wg.Add(4)
		go func() {
			_path := filepath.Join(path, dir, "nodes.dmp")
			_pathGz := _path + ".gz"
			if existed, err := pathutil.Exists(_pathGz); err != nil {
				checkError(fmt.Errorf("checking %s: %s", _pathGz, err))
			} else if existed {
				taxid2lineageTaxids, taxid2rank = getTaxid2LineageTaxids(_pathGz)
			} else {
				taxid2lineageTaxids, taxid2rank = getTaxid2LineageTaxids(_path)
			}
			wg.Done()
		}()
		go func() {
			_path := filepath.Join(path, dir, "names.dmp")
			_pathGz := _path + ".gz"
			if existed, err := pathutil.Exists(_pathGz); err != nil {
				checkError(fmt.Errorf("checking %s: %s", _pathGz, err))
			} else if existed {
				taxid2name = getTaxonNames(_pathGz)
			} else {
				taxid2name = getTaxonNames(_path)
			}
			wg.Done()
		}()
		go func() {
			_path := filepath.Join(path, dir, "delnodes.dmp")
			_pathGz := _path + ".gz"
			if existed, err := pathutil.Exists(_pathGz); err != nil {
				checkError(fmt.Errorf("checking %s: %s", _pathGz, err))
			} else if existed {
				delTaxids = getDelnodes(_pathGz)
			} else {
				delTaxids = getDelnodes(_path)
			}
			wg.Done()
		}()
		go func() {
			_path := filepath.Join(path, dir, "merged.dmp")
			_pathGz := _path + ".gz"
			if existed, err := pathutil.Exists(_pathGz); err != nil {
				checkError(fmt.Errorf("checking %s: %s", _pathGz, err))
			} else if existed {
				merges = getMergedNodes(_pathGz)
			} else {
				merges = getMergedNodes(_path)
			}
			wg.Done()
		}()
		wg.Wait()

		// -------------- checking newly added and lineage-changed taxids --------------
		if config.Verbose {
			log.Infof("  checking newly added and lineage-changed taxids")
		}

		taxid2names[int16(version)] = taxid2name
		taxid2ranks[int16(version)] = taxid2rank

		for taxid, lineageTaxids := range taxid2lineageTaxids {
			if changes, ok = data[taxid]; !ok { // first record, newly added
				data[taxid] = make([]TaxidChange, 0, 1)

				data[taxid] = append(data[taxid], TaxidChange{
					Version:       int16(version),
					LineageTaxids: lineageTaxids,
					TaxidVersion:  int16(version),
					Change:        TaxidNew,
					ChangeValue:   nil,
				})
			} else { // appending changes
				prevChange = &changes[len(changes)-1]

				switch prevChange.Change {
				case TaxidDelete: // reusing deleted taxids
					data[taxid] = append(data[taxid], TaxidChange{
						Version:       int16(version),
						LineageTaxids: lineageTaxids,
						TaxidVersion:  int16(version),
						Change:        TaxidReuseDeleted,
						ChangeValue:   nil,
					})
					// log.Infof("  deleleted taxid was reused: %d", taxid)
				case TaxidMerge: /// reusing merged taxids
					// the only case is: merged taxids being independent again,
					// including 101480,36032,37769,904709,1087732,523106,1076256,1033749,220802
					data[taxid] = append(data[taxid], TaxidChange{
						Version:       int16(version),
						LineageTaxids: lineageTaxids,
						TaxidVersion:  int16(version),
						Change:        TaxidReuseMerged,
						ChangeValue:   nil,
					})
					// log.Infof("  merged taxid was reused: %d", taxid)
				default: // need to check whether lineage changed
					if prevChange.TaxidVersion < 0 { // no lineage information
						// the only case is: merged taxids being independent again,
						// including 101480,36032,37769,904709,1087732,523106,1076256,1033749,220802
						data[taxid] = append(data[taxid], TaxidChange{
							Version:       int16(version),
							LineageTaxids: lineageTaxids,
							TaxidVersion:  int16(version),
							Change:        TaxidReuseMerged,
							ChangeValue:   nil,
						})
						// log.Infof("  merged taxid was reused: %d", taxid)
					} else {
						// lineage changed
						changeCode = linegeChangeType(lineageTaxids, prevChange.LineageTaxids, taxid2names, int16(version), prevChange.TaxidVersion)
						if changeCode > 0 { // changed
							data[taxid] = append(data[taxid], TaxidChange{
								Version:       int16(version),
								LineageTaxids: lineageTaxids,
								TaxidVersion:  int16(version),
								Change:        changeCode,
								ChangeValue:   nil,
							})
						}

						// name changed
						if taxid2names[prevChange.TaxidVersion][taxid] != taxid2names[int16(version)][taxid] {
							data[taxid] = append(data[taxid], TaxidChange{
								Version:       int16(version),
								LineageTaxids: lineageTaxids,
								TaxidVersion:  int16(version),
								Change:        TaxidNameChanged,
								ChangeValue:   nil,
							})
						}

						// rank changed
						if taxid2ranks[prevChange.TaxidVersion][taxid] != taxid2ranks[int16(version)][taxid] {
							data[taxid] = append(data[taxid], TaxidChange{
								Version:       int16(version),
								LineageTaxids: lineageTaxids,
								TaxidVersion:  int16(version),
								Change:        TaxidRankChanged,
								ChangeValue:   nil,
							})
						}
					}
				}
			}
		}

		// -------------- checking deleted taxids --------------

		if config.Verbose {
			log.Infof("  checking deleted taxids")
		}

		for _, taxid := range delTaxids {
			if changes, ok = data[taxid]; !ok { // first record
				data[taxid] = make([]TaxidChange, 0, 1)

				data[taxid] = append(data[taxid], TaxidChange{
					Version:       int16(version),
					LineageTaxids: nil, // no lineage information
					TaxidVersion:  -1,
					Change:        TaxidDelete,
					ChangeValue:   nil,
				})
			} else {
				prevChange = &changes[len(changes)-1]

				if prevChange.Change != TaxidDelete {
					data[taxid] = append(data[taxid], TaxidChange{
						Version:       int16(version),
						LineageTaxids: prevChange.LineageTaxids, // using lineage of previous record
						TaxidVersion:  prevChange.TaxidVersion,
						Change:        TaxidDelete,
						ChangeValue:   nil,
					})
				}
			}
		}

		// -------------- checking merged taxids --------------

		if config.Verbose {
			log.Infof("  checking merged taxids")
		}

		for _, merge := range merges {
			from, to = merge[0], merge[1]

			toRecord = false
			if prevTo, ok = allMerges[from]; ok { // recorded
				if to != prevTo { // merged to another taxid
					toRecord = true
				}
			} else {
				toRecord = true
			}

			allMerges[from] = to
			if !toRecord {
				continue
			}

			// recording

			// recording merged taxid
			if changes, ok = data[from]; !ok { // first record
				data[from] = make([]TaxidChange, 0, 1)

				data[from] = append(data[from], TaxidChange{
					Version:       int16(version),
					LineageTaxids: nil,
					TaxidVersion:  -1,
					Change:        TaxidMerge,
					ChangeValue:   []uint32{to},
				})
			} else {
				prevChange = &changes[len(changes)-1]

				data[from] = append(data[from], TaxidChange{
					Version:       int16(version),
					LineageTaxids: prevChange.LineageTaxids, // using lineage of previous record
					TaxidVersion:  prevChange.TaxidVersion,
					Change:        TaxidMerge,
					ChangeValue:   []uint32{to},
				})
			}

			// add change to "to"

			if changes, ok = data[to]; !ok { // first record
				data[to] = make([]TaxidChange, 0, 1)

				data[to] = append(data[to], TaxidChange{
					Version:       int16(version),
					LineageTaxids: nil,
					TaxidVersion:  -1,
					Change:        TaxidAbsorb,
					ChangeValue:   []uint32{from},
				})
			} else {
				prevChange = &changes[len(changes)-1]

				if prevChange.Change == TaxidAbsorb && prevChange.Version == int16(version) {
					// append to previous ABSORB with same version
					prevChange.ChangeValue = append(prevChange.ChangeValue, from)
				} else { // append as another change
					data[to] = append(data[to], TaxidChange{
						Version:       int16(version),
						LineageTaxids: taxid2lineageTaxids[to],
						TaxidVersion:  int16(version),
						Change:        TaxidAbsorb,
						ChangeValue:   []uint32{from},
					})
				}
			}
		}
	}

	// -------------- output --------------

	header := strings.Split("taxid,version,change,change-value,name,rank,lineage,lineage-taxids", ",")
	writer.Write(header)

	var c TaxidChange
	var tmp, items []string
	var tid uint32
	var i int
	var taxid2name map[uint32]string

	// sorting taxids
	if config.Verbose {
		log.Infof("sorting %d taxids", len(data))
	}
	taxids := make([]int, len(data))
	i = 0
	for taxid := range data {
		taxids[i] = int(taxid)
		i++
	}
	sort.Ints(taxids)

	if config.Verbose {
		log.Infof("write to file: %s", config.OutFile)
	}
	for _, taxid := range taxids {
		changes = data[uint32(taxid)]

		// sort by version and then change
		sorts.Quicksort(TaxidChanges(changes))

		for _, c = range changes {
			items = make([]string, 0, len(header))

			// taxid
			items = append(items, fmt.Sprintf("%d", taxid))

			// version
			items = append(items, versions[int(c.Version)])

			// change
			items = append(items, fmt.Sprintf("%s", c.Change))

			// change value
			switch c.Change {
			case TaxidMerge:
				items = append(items, fmt.Sprintf("%d", c.ChangeValue[0]))
			case TaxidAbsorb:
				tmp = make([]string, len(c.ChangeValue))
				for i, tid = range c.ChangeValue {
					tmp[i] = fmt.Sprintf("%d", tid)
				}
				items = append(items, strings.Join(tmp, ";"))
			default:
				items = append(items, "")
			}

			// name

			if c.TaxidVersion >= 0 {
				items = append(items, taxid2names[c.TaxidVersion][uint32(taxid)])
			} else {
				items = append(items, "")
			}

			// rank
			if c.TaxidVersion >= 0 {
				items = append(items, taxid2ranks[c.TaxidVersion][uint32(taxid)])
			} else {
				items = append(items, "")
			}

			// lineage
			if c.TaxidVersion >= 0 {
				taxid2name = taxid2names[c.TaxidVersion]
				tmp = make([]string, len(c.LineageTaxids))
				for i, tid = range c.LineageTaxids {
					tmp[i] = taxid2name[tid]
				}

				items = append(items, strings.Join(tmp, ";"))
			} else {
				items = append(items, "")
			}

			// lineage-taxid
			if c.LineageTaxids != nil {
				tmp = make([]string, len(c.LineageTaxids))
				for i, tid = range c.LineageTaxids {
					tmp[i] = fmt.Sprintf("%d", tid)
				}
				items = append(items, strings.Join(tmp, ";"))
			} else {
				items = append(items, "")
			}

			writer.Write(items)
		}
	}
}

func checkArchives(config Config, path string) []string {
	checkFile(path)

	_, err := ioutil.ReadFile(path)
	// it's a file
	if err == nil {
		checkError(fmt.Errorf("archive path should be a directory: %s", path))
	}

	// it's a directory
	files, err := ioutil.ReadDir(path)
	if err != nil {
		log.Warning(err)
	}

	var filename string
	dirs := make([]string, 0, len(files))
	for _, file := range files {
		filename = file.Name()

		if filename[0] == '.' {
			continue
		}

		if file.IsDir() {
			dirs = append(dirs, filename)
		}
	}
	if len(dirs) == 0 {
		checkError(fmt.Errorf("no unzipped directories found in path: %s", path))
	}
	for _, dir := range dirs {
		checkFile(filepath.Join(path, dir, "names.dmp"))
		checkFile(filepath.Join(path, dir, "nodes.dmp"))
		checkFile(filepath.Join(path, dir, "delnodes.dmp"))
		checkFile(filepath.Join(path, dir, "merged.dmp"))
	}

	if config.Verbose {
		log.Infof("%d archieves found in path: %s", len(dirs), path)
	}

	return dirs
}

func checkFile(file string) {
	if exists, err := pathutil.Exists(file); err != nil {
		checkError(fmt.Errorf("checking %s: %s", file, err))
	} else if !exists {
		if exists, err := pathutil.Exists(file + ".gz"); err != nil {
			checkError(fmt.Errorf("checking %s: %s", file+".gz", err))
		} else if !exists {
			checkError(fmt.Errorf("neither %s or %s found in %s", filepath.Base(file), filepath.Base(file)+".gz", filepath.Dir(file)))
		}
	}
}
