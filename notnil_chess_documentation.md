package chess // import "github.com/notnil/chess"

Package chess is a go library designed to accomplish the following:
  - chess game / turn management
  - move validation
  - PGN encoding / decoding
  - FEN encoding / decoding

Using Moves

    game := chess.NewGame()
    moves := game.ValidMoves()
    game.Move(moves[0])

Using Algebraic Notation

    game := chess.NewGame()
    game.MoveStr("e4")

Using PGN

    pgn, _ := chess.PGN(pgnReader)
    game := chess.NewGame(pgn)

Using FEN

    fen, _ := chess.FEN("rnbqkbnr/pppppppp/8/8/8/8/PPPPPPPP/RNBQKBNR w KQkq - 0 1")
    game := chess.NewGame(fen)

Random Game

    package main

    import (
        "fmt"
        "math/rand"

        "github.com/notnil/chess"
    )

    func main() {
        game := chess.NewGame()
        // generate moves until game is over
        for game.Outcome() == chess.NoOutcome {
            // select a random move
            moves := game.ValidMoves()
            move := moves[rand.Intn(len(moves))]
            game.Move(move)
        }
        // print outcome and game PGN
        fmt.Println(game.Position().Board().Draw())
        fmt.Printf("Game completed. %s by %s.\n", game.Outcome(), game.Method())
        fmt.Println(game.String())
    }

FUNCTIONS

func FEN(fen string) (func(*Game), error)
    FEN takes a string and returns a function that updates the game to reflect
    the FEN data. Since FEN doesn't encode prior moves, the move list will
    be empty. The returned function is designed to be used in the NewGame
    constructor. An error is returned if there is a problem parsing the FEN
    data.

func PGN(r io.Reader) (func(*Game), error)
    PGN takes a reader and returns a function that updates the game to reflect
    the PGN data. The PGN can use any move notation supported by this package.
    The returned function is designed to be used in the NewGame constructor.
    An error is returned if there is a problem parsing the PGN data.

func TagPairs(tagPairs []*TagPair) func(*Game)
    TagPairs returns a function that sets the tag pairs to the given value.
    The returned function is designed to be used in the NewGame constructor.

func UseNotation(n Notation) func(*Game)
    UseNotation returns a function that sets the game's notation to the given
    value. The notation is used to parse the string supplied to the MoveStr()
    method as well as the any PGN output. The returned function is designed to
    be used in the NewGame constructor.


TYPES

type AlgebraicNotation struct{}
    AlgebraicNotation (or Standard Algebraic Notation) is the official chess
    notation used by FIDE. Examples: e4, e5, O-O (short castling), e8=Q
    (promotion)

func (AlgebraicNotation) Decode(pos *Position, s string) (*Move, error)
    Decode implements the Decoder interface.

func (AlgebraicNotation) Encode(pos *Position, m *Move) string
    Encode implements the Encoder interface.

func (AlgebraicNotation) String() string
    String implements the fmt.Stringer interface and returns the notation's
    name.

type Board struct {
	// Has unexported fields.
}
    A Board represents a chess board and its relationship between squares and
    pieces.

func NewBoard(m map[Square]Piece) *Board
    NewBoard returns a board from a square to piece mapping.

func (b *Board) Draw() string
    Draw returns visual representation of the board useful for debugging.

func (b *Board) Flip(fd FlipDirection) *Board
    Flip flips the board over the vertical or hoizontal center line.

func (b *Board) MarshalBinary() (data []byte, err error)
    MarshalBinary implements the encoding.BinaryMarshaler interface and returns
    the bitboard representations as a array of bytes. Bitboads are encoded
    in the following order: WhiteKing, WhiteQueen, WhiteRook, WhiteBishop,
    WhiteKnight WhitePawn, BlackKing, BlackQueen, BlackRook, BlackBishop,
    BlackKnight, BlackPawn

func (b *Board) MarshalText() (text []byte, err error)
    MarshalText implements the encoding.TextMarshaler interface and returns a
    string in the FEN board format: rnbqkbnr/pppppppp/8/8/8/8/PPPPPPPP/RNBQKBNR

func (b *Board) Piece(sq Square) Piece
    Piece returns the piece for the given square.

func (b *Board) Rotate() *Board
    Rotate rotates the board 90 degrees clockwise.

func (b *Board) SquareMap() map[Square]Piece
    SquareMap returns a mapping of squares to pieces. A square is only added to
    the map if it is occupied.

func (b *Board) String() string
    String implements the fmt.Stringer interface and returns a string in the FEN
    board format: rnbqkbnr/pppppppp/8/8/8/8/PPPPPPPP/RNBQKBNR

func (b *Board) Transpose() *Board
    Transpose flips the board over the A8 to H1 diagonal.

func (b *Board) UnmarshalBinary(data []byte) error
    UnmarshalBinary implements the encoding.BinaryUnmarshaler interface
    and parses the bitboard representations as a array of bytes. Bitboads
    are decoded in the following order: WhiteKing, WhiteQueen, WhiteRook,
    WhiteBishop, WhiteKnight WhitePawn, BlackKing, BlackQueen, BlackRook,
    BlackBishop, BlackKnight, BlackPawn

func (b *Board) UnmarshalText(text []byte) error
    UnmarshalText implements the encoding.TextUnarshaler interface and takes a
    string in the FEN board format: rnbqkbnr/pppppppp/8/8/8/8/PPPPPPPP/RNBQKBNR

type CastleRights string
    CastleRights holds the state of both sides castling abilities.

func (cr CastleRights) CanCastle(c Color, side Side) bool
    CanCastle returns true if the given color and side combination can castle,
    otherwise returns false.

func (cr CastleRights) String() string
    String implements the fmt.Stringer interface and returns a FEN compatible
    string. Ex. KQq

type Color int8
    Color represents the color of a chess piece.

const (
	// NoColor represents no color
	NoColor Color = iota
	// White represents the color white
	White
	// Black represents the color black
	Black
)
func (c Color) Name() string
    Name returns a display friendly name.

func (c Color) Other() Color
    Other returns the opposite color of the receiver.

func (c Color) String() string
    String implements the fmt.Stringer interface and returns the color's FEN
    compatible notation.

type Decoder interface {
	Decode(pos *Position, s string) (*Move, error)
}
    Decoder is the interface implemented by objects that can decode a string
    into a move given the position. It is not the decoders responsibility to
    validate the move. An error is returned if the string could not be decoded.

type Encoder interface {
	Encode(pos *Position, m *Move) string
}
    Encoder is the interface implemented by objects that can encode a move
    into a string given the position. It is not the encoders responsibility to
    validate the move.

type File int8
    A File is the file of a square.

const (
	FileA File = iota
	FileB
	FileC
	FileD
	FileE
	FileF
	FileG
	FileH
)
func (f File) String() string

type FlipDirection int
    FlipDirection is the direction for the Board.Flip method

const (
	// UpDown flips the board's rank values
	UpDown FlipDirection = iota
	// LeftRight flips the board's file values
	LeftRight
)
type Game struct {
	// Has unexported fields.
}
    A Game represents a single chess game.

func GamesFromPGN(r io.Reader) ([]*Game, error)
    GamesFromPGN returns all PGN decoding games from the reader. It is designed
    to be used decoding multiple PGNs in the same file. An error is returned if
    there is an issue parsing the PGNs. Deprecated: Use Scanner instead.

func NewGame(options ...func(*Game)) *Game
    NewGame defaults to returning a game in the standard opening position.
    Options can be given to configure the game's initial state.

func (g *Game) AddTagPair(k, v string) bool
    AddTagPair adds or updates a tag pair with the given key and value and
    returns true if the value is overwritten.

func (g *Game) Clone() *Game

func (g *Game) Comments() [][]string
    Comments returns the comments for the game indexed by moves.

func (g *Game) Draw(method Method) error
    Draw attempts to draw the game by the given method. If the method is valid,
    then the game is updated to a draw by that method. If the method isn't valid
    then an error is returned.

func (g *Game) EligibleDraws() []Method
    EligibleDraws returns valid inputs for the Draw() method.

func (g *Game) FEN() string
    FEN returns the FEN notation of the current position.

func (g *Game) GetTagPair(k string) *TagPair
    GetTagPair returns the tag pair for the given key or nil if it is not
    present.

func (g *Game) MarshalText() (text []byte, err error)
    MarshalText implements the encoding.TextMarshaler interface and encodes the
    game's PGN.

func (g *Game) Method() Method
    Method returns the method in which the outcome occurred.

func (g *Game) Move(m *Move) error
    Move updates the game with the given move. An error is returned if the move
    is invalid or the game has already been completed.

func (g *Game) MoveHistory() []*MoveHistory
    MoveHistory returns the moves in order along with the pre and post positions
    and any comments.

func (g *Game) MoveStr(s string) error
    MoveStr decodes the given string in game's notation and calls the Move
    function. An error is returned if the move can't be decoded or the move is
    invalid.

func (g *Game) Moves() []*Move
    Moves returns the move history of the game.

func (g *Game) Outcome() Outcome
    Outcome returns the game outcome.

func (g *Game) Position() *Position
    Position returns the game's current position.

func (g *Game) Positions() []*Position
    Positions returns the position history of the game.

func (g *Game) RemoveTagPair(k string) bool
    RemoveTagPair removes the tag pair for the given key and returns true if a
    tag pair was removed.

func (g *Game) Resign(color Color)
    Resign resigns the game for the given color. If the game has already been
    completed then the game is not updated.

func (g *Game) String() string
    String implements the fmt.Stringer interface and returns the game's PGN.

func (g *Game) TagPairs() []*TagPair
    TagPairs returns the game's tag pairs.

func (g *Game) UnmarshalText(text []byte) error
    UnmarshalText implements the encoding.TextUnarshaler interface and assumes
    the data is in the PGN format.

func (g *Game) ValidMoves() []*Move
    ValidMoves returns a list of valid moves in the current position.

type LongAlgebraicNotation struct{}
    LongAlgebraicNotation is a fully expanded version of algebraic notation in
    which the starting and ending squares are specified. Examples: e2e4, Rd3xd7,
    O-O (short castling), e7e8=Q (promotion)

func (LongAlgebraicNotation) Decode(pos *Position, s string) (*Move, error)
    Decode implements the Decoder interface.

func (LongAlgebraicNotation) Encode(pos *Position, m *Move) string
    Encode implements the Encoder interface.

func (LongAlgebraicNotation) String() string
    String implements the fmt.Stringer interface and returns the notation's
    name.

type Method uint8
    A Method is the method that generated the outcome.

const (
	// NoMethod indicates that an outcome hasn't occurred or that the method can't be determined.
	NoMethod Method = iota
	// Checkmate indicates that the game was won checkmate.
	Checkmate
	// Resignation indicates that the game was won by resignation.
	Resignation
	// DrawOffer indicates that the game was drawn by a draw offer.
	DrawOffer
	// Stalemate indicates that the game was drawn by stalemate.
	Stalemate
	// ThreefoldRepetition indicates that the game was drawn when the game
	// state was repeated three times and a player requested a draw.
	ThreefoldRepetition
	// FivefoldRepetition indicates that the game was automatically drawn
	// by the game state being repeated five times.
	FivefoldRepetition
	// FiftyMoveRule indicates that the game was drawn by the half
	// move clock being one hundred or greater when a player requested a draw.
	FiftyMoveRule
	// SeventyFiveMoveRule indicates that the game was automatically drawn
	// when the half move clock was one hundred and fifty or greater.
	SeventyFiveMoveRule
	// InsufficientMaterial indicates that the game was automatically drawn
	// because there was insufficient material for checkmate.
	InsufficientMaterial
)
func (i Method) String() string

type Move struct {
	// Has unexported fields.
}
    A Move is the movement of a piece from one square to another.

func (m *Move) HasTag(tag MoveTag) bool
    HasTag returns true if the move contains the MoveTag given.

func (m *Move) Promo() PieceType
    Promo returns promotion piece type of the move.

func (m *Move) S1() Square
    S1 returns the origin square of the move.

func (m *Move) S2() Square
    S2 returns the destination square of the move.

func (m *Move) String() string
    String returns a string useful for debugging. String doesn't return
    algebraic notation.

type MoveHistory struct {
	PrePosition  *Position
	PostPosition *Position
	Move         *Move
	Comments     []string
}
    MoveHistory is a move's result from Game's MoveHistory method. It contains
    the move itself, any comments, and the pre and post positions.

type MoveTag uint16
    A MoveTag represents a notable consequence of a move.

const (
	// KingSideCastle indicates that the move is a king side castle.
	KingSideCastle MoveTag = 1 << iota
	// QueenSideCastle indicates that the move is a queen side castle.
	QueenSideCastle
	// Capture indicates that the move captures a piece.
	Capture
	// EnPassant indicates that the move captures via en passant.
	EnPassant
	// Check indicates that the move puts the opposing player in check.
	Check
)
type Notation interface {
	Encoder
	Decoder
}
    Notation is the interface implemented by objects that can encode and decode
    moves.

type Outcome string
    A Outcome is the result of a game.

const (
	// NoOutcome indicates that a game is in progress or ended without a result.
	NoOutcome Outcome = "*"
	// WhiteWon indicates that white won the game.
	WhiteWon Outcome = "1-0"
	// BlackWon indicates that black won the game.
	BlackWon Outcome = "0-1"
	// Draw indicates that game was a draw.
	Draw Outcome = "1/2-1/2"
)
func (o Outcome) String() string
    String implements the fmt.Stringer interface

type Piece int8
    Piece is a piece type with a color.

const (
	// NoPiece represents no piece
	NoPiece Piece = iota
	// WhiteKing is a white king
	WhiteKing
	// WhiteQueen is a white queen
	WhiteQueen
	// WhiteRook is a white rook
	WhiteRook
	// WhiteBishop is a white bishop
	WhiteBishop
	// WhiteKnight is a white knight
	WhiteKnight
	// WhitePawn is a white pawn
	WhitePawn
	// BlackKing is a black king
	BlackKing
	// BlackQueen is a black queen
	BlackQueen
	// BlackRook is a black rook
	BlackRook
	// BlackBishop is a black bishop
	BlackBishop
	// BlackKnight is a black knight
	BlackKnight
	// BlackPawn is a black pawn
	BlackPawn
)
func NewPiece(t PieceType, c Color) Piece
    NewPiece returns the piece matching the PieceType and Color. NoPiece is
    returned if the PieceType or Color isn't valid.

func (p Piece) Color() Color
    Color returns the color of the piece.

func (p Piece) String() string
    String implements the fmt.Stringer interface

func (p Piece) Type() PieceType
    Type returns the type of the piece.

type PieceType int8
    PieceType is the type of a piece.

const (
	// NoPieceType represents a lack of piece type
	NoPieceType PieceType = iota
	// King represents a king
	King
	// Queen represents a queen
	Queen
	// Rook represents a rook
	Rook
	// Bishop represents a bishop
	Bishop
	// Knight represents a knight
	Knight
	// Pawn represents a pawn
	Pawn
)
func PieceTypes() [6]PieceType
    PieceTypes returns a slice of all piece types.

func (p PieceType) String() string

type Position struct {
	// Has unexported fields.
}
    Position represents the state of the game without reguard to its outcome.
    Position is translatable to FEN notation.

func StartingPosition() *Position
    StartingPosition returns the starting position
    rnbqkbnr/pppppppp/8/8/8/8/PPPPPPPP/RNBQKBNR w KQkq - 0 1

func (pos *Position) Board() *Board
    Board returns the position's board.

func (pos *Position) CastleRights() CastleRights
    CastleRights returns the castling rights of the position.

func (pos *Position) EnPassantSquare() Square
    EnPassantSquare returns the en-passant square.

func (pos *Position) HalfMoveClock() int
    HalfMoveClock returns the half-move clock (50-rule).

func (pos *Position) Hash() [16]byte
    Hash returns a unique hash of the position

func (pos *Position) MarshalBinary() (data []byte, err error)
    MarshalBinary implements the encoding.BinaryMarshaler interface

func (pos *Position) MarshalText() (text []byte, err error)
    MarshalText implements the encoding.TextMarshaler interface and encodes the
    position's FEN.

func (pos *Position) Status() Method
    Status returns the position's status as one of the outcome methods. Possible
    returns values include Checkmate, Stalemate, and NoMethod.

func (pos *Position) String() string
    String implements the fmt.Stringer interface and returns a string with the
    FEN format: rnbqkbnr/pppppppp/8/8/8/8/PPPPPPPP/RNBQKBNR w KQkq - 0 1

func (pos *Position) Turn() Color
    Turn returns the color to move next.

func (pos *Position) UnmarshalBinary(data []byte) error
    UnmarshalBinary implements the encoding.BinaryMarshaler interface

func (pos *Position) UnmarshalText(text []byte) error
    UnmarshalText implements the encoding.TextUnarshaler interface and assumes
    the data is in the FEN format.

func (pos *Position) Update(m *Move) *Position
    Update returns a new position resulting from the given move. The move
    itself isn't validated, if validation is needed use Game's Move method.
    This method is more performant for bots that rely on the ValidMoves because
    it skips redundant validation.

func (pos *Position) ValidMoves() []*Move
    ValidMoves returns a list of valid moves for the position.

type Rank int8
    A Rank is the rank of a square.

const (
	Rank1 Rank = iota
	Rank2
	Rank3
	Rank4
	Rank5
	Rank6
	Rank7
	Rank8
)
func (r Rank) String() string

type Scanner struct {
	// Has unexported fields.
}
    Scanner is modeled on the bufio.Scanner type but instead of reading lines,
    it reads chess games from concatenated PGN files. It is designed to replace
    GamesFromPGN in order to handle very large PGN database files such as
    https://database.lichess.org/.

func NewScanner(r io.Reader) *Scanner
    NewScanner returns a new scanner.

func (s *Scanner) Err() error
    Err returns an error encountered during scanning. Typically this will be a
    PGN parsing error or an io.EOF.

func (s *Scanner) Next() *Game
    Next returns the game from the most recent Scan.

func (s *Scanner) Scan() bool
    Scan returns false if there was an error parsing a game or EOF was reached.
    Running scan populates data for Next() and Err().

type Side int
    Side represents a side of the board.

const (
	// KingSide is the right side of the board from white's perspective.
	KingSide Side = iota + 1
	// QueenSide is the left side of the board from white's perspective.
	QueenSide
)
type Square int8
    A Square is one of the 64 rank and file combinations that make up a chess
    board.

const (
	NoSquare Square = iota - 1
	A1
	B1
	C1
	D1
	E1
	F1
	G1
	H1
	A2
	B2
	C2
	D2
	E2
	F2
	G2
	H2
	A3
	B3
	C3
	D3
	E3
	F3
	G3
	H3
	A4
	B4
	C4
	D4
	E4
	F4
	G4
	H4
	A5
	B5
	C5
	D5
	E5
	F5
	G5
	H5
	A6
	B6
	C6
	D6
	E6
	F6
	G6
	H6
	A7
	B7
	C7
	D7
	E7
	F7
	G7
	H7
	A8
	B8
	C8
	D8
	E8
	F8
	G8
	H8
)
func NewSquare(f File, r Rank) Square
    NewSquare creates a new Square from a File and a Rank

func (sq Square) File() File
    File returns the square's file.

func (sq Square) Rank() Rank
    Rank returns the square's rank.

func (sq Square) String() string

type TagPair struct {
	Key   string
	Value string
}
    TagPair represents metadata in a key value pairing used in the PGN format.

type UCINotation struct{}
    UCINotation is a more computer friendly alternative to algebraic notation.
    This notation uses the same format as the UCI (Universal Chess Interface).
    Examples: e2e4, e7e5, e1g1 (white short castling), e7e8q (for promotion)

func (UCINotation) Decode(pos *Position, s string) (*Move, error)
    Decode implements the Decoder interface.

func (UCINotation) Encode(pos *Position, m *Move) string
    Encode implements the Encoder interface.

func (UCINotation) String() string
    String implements the fmt.Stringer interface and returns the notation's
    name.

